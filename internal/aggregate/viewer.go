package aggregate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

const authTimeout = 5 * time.Second
const publicMessageLimit = 1 << 20

type nativeMessage struct {
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type viewer struct {
	gateway     *Gateway
	ctx         context.Context
	cancel      context.CancelFunc
	public      *websocket.Conn
	output      *outputQueue
	writerDone  chan struct{}
	done        chan struct{}
	stopOnce    sync.Once
	terminalMu  sync.Mutex
	closeCode   int
	closeReason string
	// container/key are assigned under gateway.mu, read there by invalidation.
	container                   string
	key                         sessionKey
	native                      *websocket.Conn
	nativeSend                  chan nativeMessage
	nativeWriterDone            chan struct{}
	peer                        Peer
	stateMu                     sync.Mutex
	state                       viewerState
	inputMu                     sync.Mutex
	keys                        map[string]uint32
	buttons                     map[int]bool
	ready                       bool
	cursorMu                    sync.Mutex
	cursorImage, cursorPosition map[string]any
}

func keyFor(container, token string) sessionKey {
	return sessionKey{container: container, token: sha256.Sum256([]byte(token))}
}

func (g *Gateway) register(v *viewer) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.viewers[v] = struct{}{}
	g.wg.Add(1)
	return true
}

func (g *Gateway) bind(v *viewer, container, token string) bool {
	g.mu.Lock()
	if g.closed || v.ctx.Err() != nil {
		g.mu.Unlock()
		return false
	}
	v.container = container
	v.key = keyFor(container, token)
	old := g.bySession[v.key]
	g.bySession[v.key] = v
	g.mu.Unlock()
	if old != nil {
		old.stop(websocket.CloseNormalClosure, "viewer connection replaced")
		// Finish old control-release and normal-close BEFORE the replacement
		// opens its native WS. Otherwise old cleanup can clear the new host.
		select {
		case <-old.done:
		case <-v.ctx.Done():
			return false
		}
	}
	return v.ctx.Err() == nil
}

func (v *viewer) stop(code int, reason string) {
	v.stopOnce.Do(func() {
		v.terminalMu.Lock()
		v.closeCode = code
		v.closeReason = reason
		v.terminalMu.Unlock()
		v.output.close()
		v.cancel()
		// Interrupt an in-flight public write/read even when the remote stops
		// reading. Usually the serialized writer closes gracefully first.
		time.AfterFunc(100*time.Millisecond, func() { v.public.Close() })
	})
}

func (v *viewer) finish() {
	v.stop(websocket.CloseNormalClosure, "viewer disconnected")
	if v.peer != nil {
		_ = v.peer.Close()
	}
	if v.nativeWriterDone != nil {
		<-v.nativeWriterDone
	}
	if v.native != nil {
		v.native.Close()
	}
	<-v.writerDone
	g := v.gateway
	g.mu.Lock()
	if g.bySession[v.key] == v {
		delete(g.bySession, v.key)
	}
	delete(g.viewers, v)
	close(v.done)
	g.mu.Unlock()
	g.wg.Done()
}

func (g *Gateway) stream(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		methodError(w, "GET")
		return
	}
	if g.factory == nil {
		writeError(w, 503, "private media adapter is not installed", "not_ready")
		return
	}
	upgrader := websocket.Upgrader{HandshakeTimeout: authTimeout, CheckOrigin: g.originAllowed, Error: func(w http.ResponseWriter, r *http.Request, status int, _ error) {
		writeError(w, status, "viewer WebSocket upgrade rejected", "upgrade_failed")
	}}
	public, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(g.ctx)
	v := &viewer{gateway: g, ctx: ctx, cancel: cancel, public: public, output: newOutputQueue(), done: make(chan struct{}), writerDone: make(chan struct{}), keys: map[string]uint32{}, buttons: map[int]bool{}}
	if !g.register(v) {
		cancel()
		public.Close()
		return
	}
	go v.writePublic()
	defer v.finish()
	requestCancel := context.AfterFunc(r.Context(), func() { v.stop(websocket.CloseGoingAway, "request canceled") })
	defer requestCancel()

	// Do not perform room lookup, whoami, native WS or media allocation until
	// an explicit bounded first message supplies the room token.
	public.SetReadLimit(4096)
	_ = public.SetReadDeadline(time.Now().Add(authTimeout))
	kind, data, err := public.ReadMessage()
	var auth struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if err != nil || kind != websocket.TextMessage || decodeJSON(data, &auth) != nil || auth.Type != "auth" || !roomToken.MatchString(auth.Token) {
		v.stop(websocket.ClosePolicyViolation, "first message must authenticate with a native room token")
		return
	}
	public.SetReadLimit(publicMessageLimit)
	_ = public.SetReadDeadline(time.Now().Add(90 * time.Second))
	public.SetPongHandler(func(string) error { return public.SetReadDeadline(time.Now().Add(90 * time.Second)) })
	entry, status := g.lookup(v.ctx, name)
	if status != 0 {
		v.stop(websocket.ClosePolicyViolation, "managed HTTP room unavailable")
		return
	}
	// Register the token before the HTTP auth call, so logout/shutdown also
	// interrupts a pending whoami/upgrade and cannot miss its future peer.
	if !g.bind(v, entry.ID, auth.Token) {
		return
	}
	identity, err := g.whoami(v.ctx, entry.ID, auth.Token)
	if err != nil {
		v.stop(websocket.ClosePolicyViolation, "native room authentication or viewing permission denied")
		return
	}
	native, err := g.dialNative(v, auth.Token)
	if err != nil {
		v.stop(websocket.CloseTryAgainLater, "native room signaling unavailable")
		return
	}
	v.native = native
	native.SetReadLimit(maxNativeJSON)
	_ = native.SetReadDeadline(time.Now().Add(authTimeout))
	v.nativeSend = make(chan nativeMessage, 64)
	v.nativeWriterDone = make(chan struct{})
	go v.writeNative()
	for {
		var message nativeMessage
		if native.ReadJSON(&message) != nil {
			v.stop(websocket.ClosePolicyViolation, "native room rejected viewer connection")
			return
		}
		traceNative("receive", message)
		if message.Event == "system/heartbeat" {
			if v.sendNative("client/heartbeat", nil) != nil {
				return
			}
			continue
		}
		// A successful native upgrade is NOT authentication. In particular,
		// system/disconnect is the normal invalid-token upgrade response.
		if message.Event != "system/init" {
			v.stop(websocket.ClosePolicyViolation, "native room did not authenticate viewer")
			return
		}
		if err := v.initialize(identity, message.Payload); err != nil {
			v.stop(websocket.ClosePolicyViolation, err.Error())
			return
		}
		break
	}
	_ = native.SetReadDeadline(time.Time{})
	v.peer, err = g.factory.NewPeer(v.ctx, PeerCallbacks{
		Signal: v.sendNative, Ready: v.mediaReady, Media: v.media, Data: v.cursor,
		Fail: func(err error) {
			log.Error().Err(err).Str("module", "aggregate").Msg("private media peer failed")
			v.stop(websocket.CloseTryAgainLater, "private media stream failed")
		},
	})
	if err != nil || v.peer == nil {
		v.stop(websocket.CloseTryAgainLater, "private media adapter unavailable")
		return
	}
	if err := v.sendNative("signal/request", map[string]any{"video": map[string]bool{"auto": false}, "audio": map[string]bool{"disabled": false}}); err != nil {
		return
	}
	inputDone := make(chan struct{})
	go func() { defer close(inputDone); v.readPublic() }()
	defer func() { v.stop(websocket.CloseNormalClosure, "viewer disconnected"); <-inputDone }()
	for v.ctx.Err() == nil {
		var message nativeMessage
		if err := native.ReadJSON(&message); err != nil {
			v.stop(websocket.CloseGoingAway, "native room disconnected")
			return
		}
		traceNative("receive", message)
		if err := v.handleNative(message); err != nil {
			log.Error().Err(err).Str("module", "aggregate").Str("event", message.Event).Msg("private native event failed")
			return
		}
	}
}

func (v *viewer) sendNative(event string, payload any) error {
	if v.ctx.Err() != nil {
		return v.ctx.Err()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	select {
	case v.nativeSend <- nativeMessage{Event: event, Payload: body}:
		return nil
	case <-v.ctx.Done():
		return v.ctx.Err()
	default:
		v.stop(websocket.CloseTryAgainLater, "native signaling queue overflow")
		return ErrSlowConsumer
	}
}

func (v *viewer) writeNative() {
	defer close(v.nativeWriterDone)
	for {
		if v.ctx.Err() != nil {
			// The same authenticated session owns both messages. Native normal
			// close immediately resets held keys/host; abnormal close waits 5s.
			_ = v.native.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
			_ = v.native.WriteJSON(nativeMessage{Event: "control/release"})
			_ = v.native.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "viewer closed"), time.Now().Add(200*time.Millisecond))
			time.AfterFunc(250*time.Millisecond, func() { v.native.Close() })
			return
		}
		select {
		case <-v.ctx.Done():
			continue
		case message := <-v.nativeSend:
			if v.ctx.Err() != nil {
				continue
			}
			_ = v.native.SetWriteDeadline(time.Now().Add(time.Second))
			traceNative("send", message)
			if v.native.WriteJSON(message) != nil {
				v.stop(websocket.CloseGoingAway, "native signaling write failed")
			}
		}
	}
}

func (v *viewer) emit(value any) error {
	if v.ctx.Err() != nil {
		return v.ctx.Err()
	}
	err := v.output.json(value)
	if err != nil {
		v.stop(websocket.CloseTryAgainLater, "viewer output queue overflow")
	}
	return err
}

func (v *viewer) writePublic() {
	defer close(v.writerDone)
	defer v.public.Close()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		if v.ctx.Err() != nil {
			v.terminalMu.Lock()
			code, reason := v.closeCode, v.closeReason
			v.terminalMu.Unlock()
			if code == 0 {
				code = websocket.CloseGoingAway
				reason = "manager shutting down"
			}
			_ = v.public.SetWriteDeadline(time.Now().Add(75 * time.Millisecond))
			if code == websocket.ClosePolicyViolation || code == websocket.CloseUnsupportedData {
				_ = v.public.WriteJSON(map[string]string{"type": "error", "message": reason, "code": "stream_closed"})
			}
			_ = v.public.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(75*time.Millisecond))
			return
		}
		frame, ok, err := v.output.pop()
		if errors.Is(err, ErrSlowConsumer) {
			v.stop(websocket.CloseTryAgainLater, "viewer cannot keep up with live media")
			continue
		}
		if ok {
			if v.ctx.Err() != nil {
				continue
			}
			_ = v.public.SetWriteDeadline(time.Now().Add(time.Second))
			if v.public.WriteMessage(frame.kind, frame.data) != nil {
				v.stop(websocket.CloseGoingAway, "viewer write failed")
			}
			continue
		}
		select {
		case <-v.ctx.Done():
		case <-v.output.wake:
		case <-ticker.C:
			if v.public.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)) != nil {
				v.stop(websocket.CloseGoingAway, "viewer heartbeat failed")
			}
		}
	}
}

func (v *viewer) readPublic() {
	for v.ctx.Err() == nil {
		kind, data, err := v.public.ReadMessage()
		if err != nil {
			v.stop(websocket.CloseNormalClosure, "viewer disconnected")
			return
		}
		if kind != websocket.TextMessage {
			v.stop(websocket.ClosePolicyViolation, "viewer input must be JSON text")
			return
		}
		if err := v.input(data); err != nil {
			if v.emit(map[string]string{"type": "state", "input_error": err.Error()}) != nil {
				return
			}
		}
	}
}

func (v *viewer) mediaReady(info MediaInfo) error {
	if info.MIME != `video/webm; codecs="vp8,opus"` {
		v.stop(websocket.CloseUnsupportedData, "unsupported private media codecs")
		return ErrNotReady
	}
	v.stateMu.Lock()
	if v.ready {
		v.stateMu.Unlock()
		v.stop(websocket.CloseTryAgainLater, "media epoch changed; reconnect required")
		return ErrNotReady
	}
	v.ready = true
	width, height, id := v.state.Width, v.state.Height, v.state.ID
	err := v.emit(map[string]any{"type": "hello", "mime": info.MIME, "width": width, "height": height, "member_id": id})
	v.stateMu.Unlock()
	if err != nil {
		return err
	}
	// Native sends the current cursor as soon as its DC opens, often before
	// the first video keyframe. The owned viewer resets its overlay on hello.
	v.cursorMu.Lock()
	defer v.cursorMu.Unlock()
	for _, cursor := range []map[string]any{v.cursorImage, v.cursorPosition} {
		if cursor != nil {
			if err := v.emit(cursor); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *viewer) media(data []byte) error {
	v.stateMu.Lock()
	ready := v.ready
	allowed := v.state.allowed()
	v.stateMu.Unlock()
	if v.ctx.Err() != nil || !allowed || !ready {
		return ErrNotReady
	}
	err := v.output.push(websocket.BinaryMessage, data)
	if err != nil {
		v.stop(websocket.CloseTryAgainLater, "viewer cannot keep up with live media")
	}
	return err
}
