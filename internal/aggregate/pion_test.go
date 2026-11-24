package aggregate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func TestProductionPrivatePionNegotiatesRTPWebMAndOrderedInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	dc, err := native.CreateDataChannel("data", nil)
	if err != nil {
		t.Fatal(err)
	}
	nativeInput := make(chan []byte, 4)
	dc.OnMessage(func(message webrtc.DataChannelMessage) { nativeInput <- message.Data })
	dc.OnOpen(func() { _ = dc.Send([]byte{1, 0, 7, 0, 100, 0, 200}) })
	ready := make(chan MediaInfo, 1)
	chunks := make(chan []byte, 64)
	cursor := make(chan []byte, 4)
	signals := make(chan nativeMessage, 64)
	failed := make(chan error, 4)
	peer, err := (PionFactory{}).NewPeer(ctx, PeerCallbacks{
		Signal: func(event string, payload any) error {
			data, _ := json.Marshal(payload)
			select {
			case signals <- nativeMessage{Event: event, Payload: data}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		Ready: func(info MediaInfo) error { ready <- info; return nil },
		Media: func(data []byte) error {
			select {
			case chunks <- append([]byte(nil), data...):
				return nil
			default:
				return ErrSlowConsumer
			}
		},
		Data: func(data []byte) { cursor <- append([]byte(nil), data...) }, Fail: func(err error) {
			select {
			case failed <- err:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if peer.SendData([]byte{1}) != ErrInputUnavailable {
		t.Fatal("input accepted before native channel opens")
	}
	var nativeCandidates []webrtc.ICECandidateInit
	candidatesDone := make(chan struct{})
	native.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			close(candidatesDone)
			return
		}
		nativeCandidates = append(nativeCandidates, candidate.ToJSON())
	})
	offer, err := native.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = native.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	receive(t, candidatesDone)
	if len(nativeCandidates) == 0 {
		t.Fatal("no private fixture candidates")
	}
	// Exercise the actual native ordering: candidates can precede provide.
	for _, candidate := range nativeCandidates {
		data, _ := json.Marshal(candidate)
		if err = peer.HandleSignal("signal/candidate", data); err != nil {
			t.Fatal(err)
		}
	}
	var writeMu sync.Mutex
	signalsDone := make(chan struct{})
	go func() {
		defer close(signalsDone)
		var pending []webrtc.ICECandidateInit
		remote := false
		for {
			select {
			case <-ctx.Done():
				return
			case signal := <-signals:
				writeMu.Lock()
				switch signal.Event {
				case "signal/answer":
					var answer struct {
						SDP string `json:"sdp"`
					}
					_ = json.Unmarshal(signal.Payload, &answer)
					err := native.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP})
					if err != nil {
						failed <- err
					}
					remote = true
					for _, candidate := range pending {
						if err := native.AddICECandidate(candidate); err != nil {
							failed <- err
						}
					}
					pending = nil
				case "signal/candidate":
					var candidate webrtc.ICECandidateInit
					_ = json.Unmarshal(signal.Payload, &candidate)
					if remote {
						if err := native.AddICECandidate(candidate); err != nil {
							failed <- err
						}
					} else {
						pending = append(pending, candidate)
					}
				}
				writeMu.Unlock()
			}
		}
	}()
	data, _ := json.Marshal(map[string]any{"sdp": native.LocalDescription().SDP, "iceservers": []map[string]string{{"urls": "turn:must-not-be-used.invalid"}}})
	if err = peer.HandleSignal("signal/provide", data); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, cursor); len(got) != 7 || got[0] != 1 {
		t.Fatal("native private cursor DC not received")
	}
	input := dcMove(100, 200)
	if err = peer.SendData(input); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, nativeInput); string(got) != string(input) {
		t.Fatal("private DC changed input framing")
	}
	writerDone := make(chan struct{})
	mediaStop := make(chan struct{})
	go func() {
		defer close(writerDone)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-mediaStop:
				return
			case <-ticker.C:
				// RTP/mux fixtures exercise real production negotiation, packetization,
				// assembly and EBML. This minimal VP8 header is NOT a decoder proof;
				// the separate real-desktop PoC and integration canary own decoding.
				if i%5 == 0 {
					_ = video.WriteSample(media.Sample{Data: []byte{0x10, 0, 0, 0x9d, 0x01, 0x2a, 0x80, 0x02, 0x68, 0x01, 0}, Duration: 100 * time.Millisecond})
				}
				_ = audio.WriteSample(media.Sample{Data: []byte{0xf8, 0xff, 0xfe}, Duration: 20 * time.Millisecond})
				i++
			}
		}
	}()
	select {
	case info := <-ready:
		if info.MIME != webmMIME {
			t.Fatal("wrong verified media MIME")
		}
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("private production media never initialized")
	}
	init := receive(t, chunks)
	cluster := receive(t, chunks)
	if len(init) < 4 || string(init[:4]) != "\x1a\x45\xdf\xa3" || len(cluster) < 4 || string(cluster[:4]) != "\x1f\x43\xb6\x75" {
		t.Fatal("production peer didn't emit WebM init then finite Cluster")
	}
	close(mediaStop)
	receive(t, writerDone)
	select {
	case err := <-failed:
		if !strings.Contains(err.Error(), "stalled") {
			t.Fatalf("wrong stopped-track failure: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("production peer stayed connected after its two-second media stall budget")
	}
	cancel()
	receive(t, signalsDone)
	if err = peer.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(peer.SendData(input), ErrInputUnavailable) {
		t.Fatal("closed private input accepted")
	}
}
func TestPrivateCandidatePolicyRejectsPublicNATRelayAndDNS(t *testing.T) {
	for _, candidate := range []string{
		"candidate:1 1 udp 1 8.8.8.8 5000 typ host",
		"candidate:1 1 udp 1 192.168.1.2 5000 typ srflx raddr 10.0.0.2 rport 5000",
		"candidate:1 1 udp 1 10.0.0.2 5000 typ relay raddr 10.0.0.3 rport 5000",
		"candidate:1 1 udp 1 hidden.local 5000 typ host",
	} {
		if privateCandidate(candidate) == nil {
			t.Fatalf("public/native relay candidate accepted: %s", candidate)
		}
	}
	if err := privateCandidate("candidate:1 1 udp 1 172.30.1.2 5000 typ host"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer, err := (PionFactory{}).NewPeer(ctx, PeerCallbacks{Signal: func(string, any) error { return nil }, Fail: func(error) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	payload := json.RawMessage(`{"candidate":"candidate:1 1 udp 1 8.8.8.8 5000 typ host"}`)
	if err = peer.HandleSignal("signal/candidate", payload); err == nil || !strings.Contains(err.Error(), "private host") {
		t.Fatal("native public candidate reached Pion")
	}
}
