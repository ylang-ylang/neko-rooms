package aggregate

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/m1k1o/neko-rooms/internal/config"
	"github.com/m1k1o/neko-rooms/internal/types"
)

const tokenA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const tokenB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const fixtureMIME = `video/webm; codecs="vp8,opus"`

func fixtureProfile() nativeProfile {
	return nativeProfile{Name: "viewer", CanLogin: true, CanConnect: true, CanWatch: true, CanHost: true, CanAccessClipboard: true}
}

type fixtureRooms struct {
	mu      sync.Mutex
	entries map[string]types.RoomEntry
	calls   int
}

func (f *fixtureRooms) GetEntryByName(_ context.Context, name string) (*types.RoomEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	entry, ok := f.entries[name]
	if !ok {
		return nil, types.ErrRoomNotFound
	}
	return &entry, nil
}
func (f *fixtureRooms) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

type fixturePeer struct {
	cb          PeerCallbacks
	signals     chan nativeMessage
	data        chan []byte
	closed      chan struct{}
	once        sync.Once
	unavailable atomic.Bool
}

func (p *fixturePeer) HandleSignal(event string, payload json.RawMessage) error {
	p.signals <- nativeMessage{Event: event, Payload: payload}
	return nil
}
func (p *fixturePeer) SendData(data []byte) error {
	if p.unavailable.Load() {
		return ErrInputUnavailable
	}
	p.data <- append([]byte(nil), data...)
	return nil
}
func (p *fixturePeer) Close() error { p.once.Do(func() { close(p.closed) }); return nil }

type fixtureFactory struct {
	peers chan *fixturePeer
	count atomic.Int64
}

func (f *fixtureFactory) NewPeer(_ context.Context, cb PeerCallbacks) (Peer, error) {
	f.count.Add(1)
	p := &fixturePeer{cb: cb, signals: make(chan nativeMessage, 64), data: make(chan []byte, 64), closed: make(chan struct{})}
	f.peers <- p
	return p, nil
}

type fixtureNativeConn struct {
	conn     *websocket.Conn
	mu       sync.Mutex
	incoming chan nativeMessage
	closed   chan int
}

func (n *fixtureNativeConn) send(event string, payload any) error {
	data, _ := json.Marshal(payload)
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.conn.WriteJSON(nativeMessage{Event: event, Payload: data})
}

type nativeRequestRecord struct{ method, path, host, authorization, origin, cookie, query string }
type fixtureNative struct {
	mu           sync.Mutex
	profile      nativeProfile
	cookieMode   bool
	initID       string
	wsDisconnect bool
	host         string
	private      bool
	requests     []nativeRequestRecord
	conns        chan *fixtureNativeConn
	whoamiGate   chan struct{}
	upgradeGate  chan struct{}
	active       map[string]bool
}

func (n *fixtureNative) serve(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	n.requests = append(n.requests, nativeRequestRecord{r.Method, r.URL.Path, r.Host, r.Header.Get("Authorization"), r.Header.Get("Origin"), r.Header.Get("Cookie"), r.URL.RawQuery})
	profile, cookieMode, initID, disconnect, host, private := n.profile, n.cookieMode, n.initID, n.wsDisconnect, n.host, n.private
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	active := n.active[token]
	n.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/login":
		var credentials struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&credentials)
		if credentials.Password != "room-password" {
			w.WriteHeader(401)
			io.WriteString(w, `{"message":"invalid password"}`)
			return
		}
		session := nativeSession{ID: "viewer-a", Token: tokenA, Profile: profile, State: json.RawMessage(`{"is_connected":false}`)}
		if cookieMode {
			session.Token = ""
			http.SetCookie(w, &http.Cookie{Name: "NEKO_SESSION", Value: "native-cookie-secret"})
		}
		_ = json.NewEncoder(w).Encode(session)
	case "/api/whoami":
		if n.whoamiGate != nil {
			select {
			case <-n.whoamiGate:
			case <-r.Context().Done():
				return
			}
		}
		if !active {
			w.WriteHeader(401)
			io.WriteString(w, `{"message":"invalid token"}`)
			return
		}
		id := "viewer-a"
		if token == tokenB {
			id = "viewer-b"
		}
		_ = json.NewEncoder(w).Encode(nativeSession{ID: id, Profile: profile, State: json.RawMessage(`{"is_connected":false}`)})
	case "/api/logout":
		if !active {
			w.WriteHeader(401)
			return
		}
		n.mu.Lock()
		n.active[token] = false
		n.mu.Unlock()
		io.WriteString(w, "true")
	case "/api/ws":
		if n.upgradeGate != nil {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			close(n.upgradeGate)
			var b [1]byte
			_, _ = conn.Read(b[:])
			return
		}
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		remote := &fixtureNativeConn{conn: conn, incoming: make(chan nativeMessage, 128), closed: make(chan int, 1)}
		n.conns <- remote
		if disconnect {
			_ = remote.send("system/disconnect", map[string]string{"message": "bad native token"})
			return
		}
		id := "viewer-a"
		if token == tokenB {
			id = "viewer-b"
		}
		if initID != "" {
			id = initID
		}
		_ = remote.send("system/init", map[string]any{"session_id": id, "screen_size": map[string]int{"width": 1280, "height": 720}, "control_host": map[string]any{"has_host": host != "", "host_id": host}, "sessions": map[string]any{id: map[string]any{"id": id, "profile": profile, "state": map[string]bool{"is_connected": true}}}, "settings": map[string]any{"private_mode": private, "heartbeat_interval": 10}, "webrtc": map[string]any{"videos": []string{"main"}}})
		for {
			var msg nativeMessage
			if err := conn.ReadJSON(&msg); err != nil {
				code := websocket.CloseAbnormalClosure
				if closeErr, ok := err.(*websocket.CloseError); ok {
					code = closeErr.Code
				}
				remote.closed <- code
				return
			}
			remote.incoming <- msg
		}
	default:
		w.WriteHeader(404)
	}
}

type harness struct {
	gateway      *Gateway
	rooms        *fixtureRooms
	native       *fixtureNative
	factory      *fixtureFactory
	server       *httptest.Server
	nativeServer *httptest.Server
}

func newHarness(t *testing.T, configure func(*fixtureNative)) *harness {
	t.Helper()
	n := &fixtureNative{profile: fixtureProfile(), conns: make(chan *fixtureNativeConn, 16), active: map[string]bool{tokenA: true, tokenB: true}}
	if configure != nil {
		configure(n)
	}
	ns := httptest.NewServer(http.HandlerFunc(n.serve))
	rooms := &fixtureRooms{entries: map[string]types.RoomEntry{"alpha": {ID: "aaaaaaaaaaaa", Name: "alpha", NetworkMode: config.NetworkModeHTTP, Running: true}, "beta": {ID: "bbbbbbbbbbbb", Name: "beta", NetworkMode: config.NetworkModeHTTP, Running: true}, "legacy": {ID: "cccccccccccc", Name: "legacy", NetworkMode: config.NetworkModeDirect, Running: true}}}
	factory := &fixtureFactory{peers: make(chan *fixturePeer, 16)}
	static := t.TempDir()
	_ = os.Mkdir(filepath.Join(static, "__aggregate"), 0755)
	_ = os.WriteFile(filepath.Join(static, "aggregate.html"), []byte("owned viewer fixture"), 0644)
	_ = os.WriteFile(filepath.Join(static, "viewer.js"), []byte("owned script fixture"), 0644)
	g := New(rooms, Options{PathPrefix: "/room", Static: static}, factory)
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "aaaaaaaaaaaa:8080" && address != "bbbbbbbbbbbb:8080" {
			t.Errorf("unscoped private address: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, ns.Listener.Addr().String())
	}
	g.client.Transport.(*http.Transport).DialContext = dial
	g.dialer.NetDialContext = dial
	s := httptest.NewServer(g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })))
	h := &harness{g, rooms, n, factory, s, ns}
	t.Cleanup(func() { g.Shutdown(); s.Close(); ns.Close() })
	return h
}
func (h *harness) call(t *testing.T, method, path, body string, headers http.Header) (int, http.Header, []byte) {
	t.Helper()
	r, _ := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	r.Header = headers
	if body != "" && r.Header.Get("Content-Type") == "" {
		r.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, data
}
func (h *harness) connect(t *testing.T, token string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.server.URL, "http")+"/room/alpha/__aggregate/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if token != "" {
		if conn.WriteJSON(map[string]string{"type": "auth", "token": token}) != nil {
			t.Fatal("write auth")
		}
	}
	return conn
}
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("fixture event timeout")
		var zero T
		return zero
	}
}
func jsonType(t *testing.T, conn *websocket.Conn, want string) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for range 20 {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind != websocket.TextMessage {
			t.Fatal("unexpected media before text event")
		}
		var msg map[string]any
		if json.Unmarshal(data, &msg) != nil {
			t.Fatal("invalid public JSON")
		}
		if msg["type"] == want {
			return msg
		}
	}
	t.Fatal("public event missing")
	return nil
}
func expectClosed(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for range 20 {
		_, _, err := conn.ReadMessage()
		if err != nil {
			if _, ok := err.(*websocket.CloseError); !ok {
				t.Fatalf("no WebSocket close: %v", err)
			}
			return
		}
	}
	t.Fatal("viewer did not close")
}

func TestNativeRoomLoginLogoutCredentialDomains(t *testing.T) {
	h := newHarness(t, nil)
	headers := http.Header{"Cookie": {"neko_rooms_admin_session=owner-secret; NEKO_SESSION=wrong-native-cookie"}, "Authorization": {"Basic b3duZXI6c2VjcmV0"}}
	status, resp, data := h.call(t, "POST", "/room/alpha/__aggregate/login", `{"username":"viewer","password":"room-password"}`, headers)
	if status != 200 || resp.Get("Set-Cookie") != "" || !strings.Contains(string(data), `"token":"`+tokenA+`"`) || !strings.Contains(string(data), `"member"`) {
		t.Fatalf("login not normalized: %d %s", status, data)
	}
	h.native.mu.Lock()
	record := h.native.requests[0]
	h.native.mu.Unlock()
	if record.path != "/api/login" || record.cookie != "" || record.authorization != "" || record.origin != "" || record.query != "" {
		t.Fatal("Owner/native cookie or authorization leaked into native room login")
	}
	status, resp, data = h.call(t, "POST", "/room/alpha/__aggregate/logout", "", http.Header{"Authorization": {"Bearer " + tokenA}})
	if status != 200 || strings.TrimSpace(string(data)) != "true" || resp.Get("WWW-Authenticate") != "" {
		t.Fatalf("logout must retain native JSON true: %d %s", status, data)
	}
	for _, path := range []string{"/room/alpha/__aggregate/renew", "/room/alpha/__aggregate/refresh"} {
		status, _, _ = h.call(t, "POST", path, "", http.Header{})
		if status != 404 {
			t.Fatal("invented native renewal endpoint")
		}
	}
}
func TestCookieModeFailsVisiblyWithoutPromotingCookie(t *testing.T) {
	h := newHarness(t, func(n *fixtureNative) { n.cookieMode = true })
	status, headers, data := h.call(t, "POST", "/room/alpha/__aggregate/login", `{"username":"viewer","password":"room-password"}`, http.Header{})
	if status != 502 || !strings.Contains(string(data), "cookie_mode_unsupported") || headers.Get("Set-Cookie") != "" {
		t.Fatalf("cookie mode bypass: %d %s", status, data)
	}
}
func TestRoutesAssetsModesAndPathValidation(t *testing.T) {
	h := newHarness(t, nil)
	for _, test := range []struct {
		path   string
		status int
		body   string
	}{
		{"/room/alpha/", 200, "owned viewer fixture"}, {"/__aggregate/viewer.js", 200, "owned script fixture"}, {"/__aggregate/", 404, ""}, {"/room/legacy/", 418, ""}, {"/room/alpha/api/room/screen/cast.jpg", 418, ""}, {"/room/missing/__aggregate/login", 405, ""},
		{"/room/alpha/__aggregate/login?token=secret", 400, ""}, {"/room/%61lpha/__aggregate/login", 400, ""}, {"/room/alpha%2fother/__aggregate/login", 400, ""}, {"/__aggregate/%2e%2e/aggregate.html", 400, ""}, {"/room/../__aggregate/login", 400, ""},
	} {
		status, headers, data := h.call(t, "GET", test.path, "", http.Header{})
		if status != test.status || (test.body != "" && !strings.Contains(string(data), test.body)) || headers.Get("WWW-Authenticate") != "" {
			t.Errorf("%s: %d %s", test.path, status, data)
		}
	}
}
func TestOriginsAndJSONLoginRejectionBeforeNativeResources(t *testing.T) {
	h := newHarness(t, nil)
	for _, test := range []struct {
		method  string
		headers http.Header
		status  int
	}{
		{"POST", http.Header{"Origin": {"https://foreign.example"}}, 403},
		{"POST", http.Header{"Origin": {h.server.URL, "https://foreign.example"}}, 403},
		{"POST", http.Header{"Content-Type": {"text/plain"}}, 415},
		{"GET", http.Header{}, 405},
	} {
		status, headers, _ := h.call(t, test.method, "/room/alpha/__aggregate/login", `{"username":"viewer","password":"room-password"}`, test.headers)
		if status != test.status || headers.Get("WWW-Authenticate") != "" {
			t.Fatalf("login policy: %d", status)
		}
	}
	_, res, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.server.URL, "http")+"/room/alpha/__aggregate/stream", http.Header{"Origin": {"https://foreign.example"}})
	if err == nil || res.StatusCode != 403 {
		t.Fatal("cross-origin viewer accepted")
	}
	res.Body.Close()
	if h.rooms.count() != 0 {
		t.Fatal("rejected requests touched room lookup")
	}
}
func TestWebSocketFirstAuthBoundedBeforePrivateResources(t *testing.T) {
	h := newHarness(t, nil)
	conn := h.connect(t, "")
	if h.rooms.count() != 0 {
		t.Fatal("unauthenticated WS allocated private resources")
	}
	if err := conn.WriteJSON(map[string]string{"type": "control", "action": "request"}); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, conn)
	if h.rooms.count() != 0 || h.factory.count.Load() != 0 {
		t.Fatal("bad first auth reached private resources")
	}
	conn = h.connect(t, "")
	if err := conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", 5000))); err != nil {
		t.Fatal(err)
	}
	expectClosed(t, conn)
	conn = h.connect(t, "")
	_ = conn.SetReadDeadline(time.Now().Add(7 * time.Second))
	start := time.Now()
	_, _, err := conn.ReadMessage()
	if err == nil {
		_, _, err = conn.ReadMessage()
	}
	if err == nil || time.Since(start) > 6*time.Second || time.Since(start) < 4*time.Second {
		t.Fatal("first auth deadline not enforced")
	}
	if h.rooms.count() != 0 || h.factory.count.Load() != 0 {
		t.Fatal("unauthenticated sockets allocated private resources")
	}
}
func TestWhoamiAndNativeInitAreBothRequired(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*fixtureNative)
	}{
		{"unknown native token", func(n *fixtureNative) { n.active[tokenA] = false }},
		{"API synthetic cannot connect", func(n *fixtureNative) { n.profile.CanConnect = false }},
		{"cannot watch", func(n *fixtureNative) { n.profile.CanWatch = false }},
		{"upgrade alone not auth", func(n *fixtureNative) { n.wsDisconnect = true }},
		{"wrong init identity", func(n *fixtureNative) { n.initID = "someone-else" }},
		{"native private mode", func(n *fixtureNative) { n.private = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, test.configure)
			conn := h.connect(t, tokenA)
			expectClosed(t, conn)
			if h.factory.count.Load() != 0 {
				t.Fatal("media created without authenticated native init")
			}
		})
	}
}
func TestAuthenticatedViewerSignalsMediaAndNativeCredentialIsolation(t *testing.T) {
	h := newHarness(t, nil)
	conn := h.connect(t, tokenA)
	state := jsonType(t, conn, "state")
	if state["member_id"] != "viewer-a" {
		t.Fatal("public member did not originate from native session")
	}
	native := receive(t, h.native.conns)
	peer := receive(t, h.factory.peers)
	message := receive(t, native.incoming)
	if message.Event != "signal/request" || !strings.Contains(string(message.Payload), `"auto":false`) {
		t.Fatal("wrong native v3 signal request")
	}
	if err := native.send("signal/candidate", map[string]string{"candidate": "private ICE fixture"}); err != nil {
		t.Fatal(err)
	}
	if receive(t, peer.signals).Event != "signal/candidate" {
		t.Fatal("native ICE not consumed by private peer")
	}
	if err := peer.cb.Signal("signal/answer", map[string]string{"sdp": "private SDP fixture"}); err != nil {
		t.Fatal(err)
	}
	if receive(t, native.incoming).Event != "signal/answer" {
		t.Fatal("private answer not serialized to native WS")
	}
	if err := peer.cb.Ready(MediaInfo{MIME: fixtureMIME, Width: 1280, Height: 720}); err != nil {
		t.Fatal(err)
	}
	hello := jsonType(t, conn, "hello")
	if hello["mime"] != fixtureMIME || hello["width"] != float64(1280) {
		t.Fatal("wrong public hello")
	}
	// Fixture bytes establish routing only, never claim functional WebM media.
	if err := peer.cb.Media([]byte("fixture-only-not-real-WebM")); err != nil {
		t.Fatal(err)
	}
	kind, data, err := conn.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(data) != "fixture-only-not-real-WebM" {
		t.Fatal("public raw binary routing failed")
	}
	h.native.mu.Lock()
	defer h.native.mu.Unlock()
	for _, r := range h.native.requests {
		if r.path == "/api/ws" || r.path == "/api/whoami" {
			if r.authorization != "Bearer "+tokenA || r.origin != "" || r.cookie != "" || r.query != "" {
				t.Fatal("native viewer credential/header isolation failed")
			}
		}
	}
}
func TestLogoutAndReplacementCloseOnlyMatchingViewer(t *testing.T) {
	h := newHarness(t, nil)
	a := h.connect(t, tokenA)
	jsonType(t, a, "state")
	na := receive(t, h.native.conns)
	pa := receive(t, h.factory.peers)
	receive(t, na.incoming)
	b := h.connect(t, tokenB)
	jsonType(t, b, "state")
	nb := receive(t, h.native.conns)
	pb := receive(t, h.factory.peers)
	receive(t, nb.incoming)
	replacement := h.connect(t, tokenA)
	jsonType(t, replacement, "state")
	expectClosed(t, a)
	receive(t, pa.closed)
	if code := receive(t, na.closed); code != websocket.CloseNormalClosure {
		t.Fatalf("old native connection didn't close normally: %d", code)
	}
	nr := receive(t, h.native.conns)
	pr := receive(t, h.factory.peers)
	receive(t, nr.incoming)
	status, _, data := h.call(t, "POST", "/room/alpha/__aggregate/logout", "", http.Header{"Authorization": {"Bearer " + tokenA}})
	if status != 200 {
		t.Fatalf("logout: %s", data)
	}
	expectClosed(t, replacement)
	receive(t, pr.closed)
	if code := receive(t, nr.closed); code != 1000 {
		t.Fatalf("logout did not normally close native WS: %d", code)
	}
	select {
	case <-pb.closed:
		t.Fatal("logout closed unrelated viewer")
	default:
	}
	if err := b.WriteJSON(map[string]string{"type": "control", "action": "request"}); err != nil {
		t.Fatal(err)
	}
	if receive(t, nb.incoming).Event != "control/request" {
		t.Fatal("unrelated viewer no longer connected")
	}
}
func TestNativePermissionAndRoomLifecycleImmediatelyDestroyViewer(t *testing.T) {
	for _, test := range []struct {
		name, event string
		payload     any
	}{
		{"profile revoked", "session/profile", map[string]any{"id": "viewer-a", "name": "viewer", "can_login": true, "can_connect": true, "can_watch": false}},
		{"private mode enabled", "system/settings", map[string]any{"private_mode": true}},
		{"native logout", "system/disconnect", map[string]any{"message": "session deleted"}},
		{"peer closed", "signal/close", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, nil)
			conn := h.connect(t, tokenA)
			jsonType(t, conn, "state")
			native := receive(t, h.native.conns)
			peer := receive(t, h.factory.peers)
			receive(t, native.incoming)
			if err := native.send(test.event, test.payload); err != nil {
				t.Fatal(err)
			}
			expectClosed(t, conn)
			receive(t, peer.closed)
			if peer.cb.Media([]byte("must-not-send")) == nil {
				t.Fatal("revoked viewer still accepts media")
			}
		})
	}
	h := newHarness(t, nil)
	conn := h.connect(t, tokenA)
	jsonType(t, conn, "state")
	peer := receive(t, h.factory.peers)
	h.gateway.stopWhere(func(v *viewer) bool { return v.container == "aaaaaaaaaaaa" }, 1001, "room recreated")
	expectClosed(t, conn)
	receive(t, peer.closed)
}
func TestShutdownCancelsWhoamiAndUnfinishedUpgrade(t *testing.T) {
	for _, phase := range []string{"whoami", "upgrade"} {
		t.Run(phase, func(t *testing.T) {
			h := newHarness(t, func(n *fixtureNative) {
				if phase == "whoami" {
					n.whoamiGate = make(chan struct{})
				} else {
					n.upgradeGate = make(chan struct{})
				}
			})
			conn := h.connect(t, tokenA)
			if phase == "upgrade" {
				receive(t, h.native.upgradeGate)
			} else {
				// The request arrives on the native fixture before shutdown.
				deadline := time.After(3 * time.Second)
				for {
					h.native.mu.Lock()
					seen := len(h.native.requests) > 0
					h.native.mu.Unlock()
					if seen {
						break
					}
					select {
					case <-deadline:
						t.Fatal("whoami did not arrive")
					default:
					}
					time.Sleep(time.Millisecond)
				}
			}
			done := make(chan struct{})
			go func() { h.gateway.Shutdown(); close(done) }()
			receive(t, done)
			expectClosed(t, conn)
			if h.factory.count.Load() != 0 {
				t.Fatal("canceled handshake created media")
			}
		})
	}
}
