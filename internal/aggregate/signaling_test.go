package aggregate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// Image4's real native trace reached provide/audio/video/candidate, but the
// permissive fixturePeer hid that production Pion rejects audio/video as SDP.
// Replay that trace through the public Gateway and the REAL private Pion peer.
// These RTP fixtures prove signaling/mux routing, not browser video decoding.
func TestNativeInitTrackAcknowledgementsReachProductionMedia(t *testing.T) {
	for _, order := range []string{"provide-first", "trickle-first", "ack-first"} {
		t.Run(order, func(t *testing.T) {
			h := newHarness(t, nil)
			h.gateway.factory = PionFactory{}
			settings := webrtc.SettingEngine{}
			settings.SetIPFilter(privateIP)
			settings.SetIncludeLoopbackCandidate(true)
			settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
			native, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
			if err != nil {
				t.Fatal(err)
			}
			defer native.Close()
			video, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "video", "desktop")
			if err != nil {
				t.Fatal(err)
			}
			audio, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "desktop")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = native.AddTrack(video); err != nil {
				t.Fatal(err)
			}
			if _, err = native.AddTrack(audio); err != nil {
				t.Fatal(err)
			}
			if _, err = native.CreateDataChannel("data", nil); err != nil {
				t.Fatal(err)
			}
			candidates := make(chan webrtc.ICECandidateInit, 16)
			native.OnICECandidate(func(candidate *webrtc.ICECandidate) {
				if candidate != nil {
					candidates <- candidate.ToJSON()
				}
			})
			gathered := webrtc.GatheringCompletePromise(native)
			offer, err := native.CreateOffer(nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = native.SetLocalDescription(offer); err != nil {
				t.Fatal(err)
			}
			receive(t, gathered)
			if len(candidates) == 0 || strings.Contains(offer.SDP, "a=candidate:") {
				t.Fatal("fixture requires native-style SDP without embedded candidates and real private trickle")
			}

			conn := h.connect(t, tokenA) // fixture sends the authenticated system/init
			state := jsonType(t, conn, "state")
			if state["can_watch"] != true || state["member_id"] != "viewer-a" {
				t.Fatal("native init authorization lost")
			}
			ws := receive(t, h.native.conns)
			if receive(t, ws.incoming).Event != "signal/request" {
				t.Fatal("missing native media request")
			}
			send := func(event string, payload any) {
				t.Helper()
				if err := ws.send(event, payload); err != nil {
					t.Fatal(err)
				}
			}
			send("session/state", map[string]any{"id": "viewer-a", "is_connected": true, "is_watching": false})
			send("filetransfer/update", map[string]any{})
			send("chat/init", []any{})
			send("openinapp/init", map[string]any{})
			audioState := map[string]any{"disabled": false}
			videoState := map[string]any{"disabled": false, "id": "main", "video": "main", "auto": false}
			sendACKs := func() {
				send("signal/audio", audioState)
				send("signal/video", videoState)
			}
			sendCandidates := func() {
				for len(candidates) > 0 {
					send("signal/candidate", <-candidates)
				}
			}
			if order == "trickle-first" {
				sendCandidates()
			}
			if order == "ack-first" {
				sendACKs()
			}
			send("signal/provide", map[string]any{"sdp": offer.SDP, "iceservers": []any{}, "audio": audioState, "video": videoState})
			sendACKs() // asynchronous native ACKs may also duplicate provide metadata
			sendCandidates()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			answered := make(chan struct{})
			failed := make(chan error, 16)
			go func() {
				remote := false
				var pending []webrtc.ICECandidateInit
				for {
					select {
					case <-ctx.Done():
						return
					case msg := <-ws.incoming:
						switch msg.Event {
						case "signal/answer":
							var answer struct{ SDP string }
							_ = json.Unmarshal(msg.Payload, &answer)
							if err := native.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
								failed <- err
								return
							}
							remote = true
							for _, candidate := range pending {
								if err := native.AddICECandidate(candidate); err != nil {
									failed <- err
									return
								}
							}
							pending = nil
							close(answered)
						case "signal/candidate":
							var candidate webrtc.ICECandidateInit
							_ = json.Unmarshal(msg.Payload, &candidate)
							if !remote {
								pending = append(pending, candidate)
							} else if err := native.AddICECandidate(candidate); err != nil {
								failed <- err
								return
							}
						}
					}
				}
			}()
			receive(t, answered)
			writerDone := make(chan struct{})
			go func() {
				defer close(writerDone)
				ticker := time.NewTicker(20 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						_ = video.WriteSample(media.Sample{Data: []byte{0x10, 0, 0, 0x9d, 0x01, 0x2a, 0x00, 0x05, 0xd0, 0x02, 0}, Duration: 20 * time.Millisecond})
						_ = audio.WriteSample(media.Sample{Data: []byte{0xf8, 0xff, 0xfe}, Duration: 20 * time.Millisecond})
					}
				}
			}()
			defer func() { cancel(); receive(t, writerDone) }()
			seenAudio, seenVideo, hello := false, false, false
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			for {
				kind, data, err := conn.ReadMessage()
				if err != nil {
					t.Fatalf("real native init sequence closed before media: %v", err)
				}
				if kind == websocket.BinaryMessage {
					if !hello || !seenAudio || !seenVideo || len(data) < 4 || string(data[:4]) != "\x1a\x45\xdf\xa3" {
						t.Fatal("missing hello, preserved track metadata or WebM init")
					}
					break
				}
				var msg map[string]any
				_ = json.Unmarshal(data, &msg)
				if strings.Contains(string(data), "sdp") || strings.Contains(string(data), "candidate") {
					t.Fatal("private signaling leaked publicly")
				}
				if msg["type"] == "hello" {
					hello = true
				}
				if a, ok := msg["audio"].(map[string]any); ok && a["disabled"] == false {
					seenAudio = true
				}
				if v, ok := msg["video"].(map[string]any); ok && v["id"] == "main" && v["video"] == "main" && v["auto"] == false && v["disabled"] == false {
					seenVideo = true
				}
			}
			select {
			case err := <-failed:
				t.Fatal(err)
			default:
			}
		})
	}
}

func TestNativeTrackStateAndRestartNeverSilentlyContinueOldEpoch(t *testing.T) {
	for _, tc := range []struct {
		name, event, payload string
	}{
		{"audio disabled", "signal/audio", `{"disabled":true}`},
		{"video disabled", "signal/video", `{"disabled":true,"id":"main","video":"main","auto":false}`},
		{"video switched", "signal/video", `{"disabled":false,"id":"other","video":"other","auto":false}`},
		{"audio malformed", "signal/audio", `{"disabled":"false"}`},
		{"audio missing status", "signal/audio", `{}`},
		{"video missing identity", "signal/video", `{"disabled":false}`},
		{"unknown signal", "signal/unrecognized", `{}`},
		{"restart", "signal/restart", `{"sdp":"new native offer"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.gateway.factory = PionFactory{}
			conn := h.connect(t, tokenA)
			jsonType(t, conn, "state")
			native := receive(t, h.native.conns)
			receive(t, native.incoming)
			if err := native.send("signal/video", map[string]any{"disabled": false, "id": "main", "video": "main", "auto": false}); err != nil {
				t.Fatal(err)
			}
			state := jsonType(t, conn, "state")
			if state["video"].(map[string]any)["id"] != "main" {
				t.Fatal("initial ACK metadata lost")
			}
			if err := native.send(tc.event, json.RawMessage(tc.payload)); err != nil {
				t.Fatal(err)
			}
			retryClose(t, conn)
			if code := receive(t, native.closed); code != websocket.CloseNormalClosure {
				t.Fatalf("track teardown did not release native session normally: %d", code)
			}
		})
	}
}
