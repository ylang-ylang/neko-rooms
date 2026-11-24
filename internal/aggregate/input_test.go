package aggregate

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"image"
	"image/png"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
)

func inputViewer() (*viewer, *fixturePeer) {
	peer := &fixturePeer{data: make(chan []byte, 64)}
	return &viewer{ctx: context.Background(), peer: peer, nativeSend: make(chan nativeMessage, 64), output: newOutputQueue(), keys: map[string]uint32{}, buttons: map[int]bool{}, state: viewerState{ID: "me", Host: "me", Profile: fixtureProfile(), Width: 1280, Height: 720, Members: map[string]nativeMember{}}}, peer
}
func TestInputUsesExactBigEndianNativeDataChannelNotHostTakingWS(t *testing.T) {
	v, peer := inputViewer()
	for _, input := range []string{
		`{"type":"pointer","action":"down","x":100,"y":200,"button":0}`,
		`{"type":"pointer","action":"up","x":100,"y":200,"button":0}`,
		`{"type":"key","action":"down","key":"a","code":"KeyA"}`,
		`{"type":"key","action":"up","key":"A","code":"KeyA"}`,
		`{"type":"pointer","action":"wheel","x":100,"y":200,"deltaX":-2,"deltaY":3}`,
	} {
		if err := v.input([]byte(input)); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"010004006400c8", "05000400000001", "010004006400c8", "06000400000001", "03000400000061", "04000400000061", "010004006400c8", "020005fffe000300"} {
		if got := hex.EncodeToString(receive(t, peer.data)); got != want {
			t.Fatalf("DC frame got %s, want %s", got, want)
		}
	}
	select {
	case message := <-v.nativeSend:
		t.Fatalf("unsafe native WS input forwarded: %s", message.Event)
	default:
	}
	for _, input := range []string{`{"type":"control","action":"request"}`, `{"type":"clipboard","text":"hello"}`, `{"type":"control","action":"release"}`} {
		if err := v.input([]byte(input)); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"control/request", "clipboard/set", "control/release"} {
		if got := receive(t, v.nativeSend).Event; got != want {
			t.Fatalf("native WS event %s, want %s", got, want)
		}
	}
}
func TestInputDropsNonhostAndUnavailableOrStaleInputWithoutFallback(t *testing.T) {
	v, peer := inputViewer()
	v.state.Host = "other"
	for _, input := range []string{`{"type":"pointer","action":"move","x":1,"y":2}`, `{"type":"key","action":"up","key":"a","code":"KeyA"}`} {
		if v.input([]byte(input)) == nil {
			t.Fatal("inactive input accepted locally")
		}
	}
	select {
	case <-peer.data:
		t.Fatal("nonhost data forwarded")
	default:
	}
	v.state.Host = "me"
	peer.unavailable.Store(true)
	if err := v.input([]byte(`{"type":"key","action":"down","key":"a","code":"KeyA"}`)); err != ErrInputUnavailable {
		t.Fatal(err)
	}
	peer.unavailable.Store(false)
	if err := v.input([]byte(`{"type":"key","action":"up","key":"a","code":"KeyA"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-peer.data:
		t.Fatal("unavailable input was replayed")
	default:
	}
	select {
	case <-v.nativeSend:
		t.Fatal("DC unavailable silently switched to host-taking WS")
	default:
	}
}
func TestInputValidatesRangesAndDOMKeysyms(t *testing.T) {
	v, _ := inputViewer()
	for _, input := range []string{
		`{"type":"pointer","action":"move","x":1280,"y":0}`, `{"type":"pointer","action":"move","x":0.5,"y":0}`, `{"type":"pointer","action":"down","x":0,"y":0,"button":9}`,
		`{"type":"pointer","action":"wheel","x":0,"y":0,"deltaX":32768,"deltaY":0}`, `{"type":"pointer","action":"move","y":0}`,
		`{"type":"key","action":"down","keyCode":65}`, `{"type":"signal/request"}`, `{"event":"control/move","payload":{"x":1,"y":2}}`,
	} {
		if v.input([]byte(input)) == nil {
			t.Fatalf("invalid input accepted: %s", input)
		}
	}
	for _, test := range []struct {
		key, code string
		want      uint32
	}{{"a", "KeyA", 0x61}, {"A", "KeyA", 0x41}, {"Enter", "Enter", 0xff0d}, {"Enter", "NumpadEnter", 0xff8d}, {"Control", "ControlRight", 0xffe4}, {"ArrowLeft", "ArrowLeft", 0xff51}, {"F12", "F12", 0xffc9}, {"é", "KeyE", 0xe9}, {"中", "KeyQ", 0x01004e2d}} {
		got, err := keysym(test.key, test.code)
		if err != nil || got != test.want {
			t.Errorf("%s/%s: %#x %v", test.key, test.code, got, err)
		}
	}
}
func TestCursorDecodesNativeAsymmetricLengthAndPNG(t *testing.T) {
	v, _ := inputViewer()
	v.cursor([]byte{1, 0, 7, 0, 100, 0, 200})
	if _, ok, _ := v.output.pop(); ok {
		t.Fatal("cursor emitted before hello resets the viewer overlay")
	}
	if err := v.mediaReady(MediaInfo{MIME: fixtureMIME}); err != nil {
		t.Fatal(err)
	}
	hello, ok, _ := v.output.pop()
	if !ok || !bytes.Contains(hello.data, []byte(`"type":"hello"`)) {
		t.Fatal("hello did not precede cached cursor")
	}
	frame, ok, err := v.output.pop()
	if err != nil || !ok || !bytes.Contains(frame.data, []byte(`"x":100`)) {
		t.Fatal("cursor position not normalized")
	}
	var out bytes.Buffer
	_ = png.Encode(&out, image.NewNRGBA(image.Rect(0, 0, 16, 16)))
	data := make([]byte, 11+out.Len())
	data[0] = 2
	binary.BigEndian.PutUint16(data[1:3], uint16(len(data)))
	binary.BigEndian.PutUint16(data[3:5], 16)
	binary.BigEndian.PutUint16(data[5:7], 16)
	binary.BigEndian.PutUint16(data[7:9], 2)
	binary.BigEndian.PutUint16(data[9:11], 3)
	copy(data[11:], out.Bytes())
	v.cursor(data)
	frame, ok, err = v.output.pop()
	if err != nil || !ok || !bytes.Contains(frame.data, []byte(`"hotspotX":2`)) || !bytes.Contains(frame.data, []byte(`"hotspotY":3`)) || !bytes.Contains(frame.data, []byte(`"png":"iVBOR`)) {
		t.Fatal("cursor PNG/hotspot not normalized")
	}
}
func TestOutputQueuePrioritizesControlAndDiscardsRevokedMedia(t *testing.T) {
	q := newOutputQueue()
	_ = q.push(websocket.BinaryMessage, []byte("media"))
	_ = q.json(map[string]string{"type": "state"})
	frame, ok, err := q.pop()
	if err != nil || !ok || frame.kind != websocket.TextMessage {
		t.Fatal("control is blocked behind queued media")
	}
	q.close()
	if _, ok, _ := q.pop(); ok || q.mediaBytes != 0 || q.media != nil {
		t.Fatal("revoked media remains deliverable")
	}
	q = newOutputQueue()
	if q.push(websocket.BinaryMessage, make([]byte, maxMediaBytes+1)) != ErrSlowConsumer {
		t.Fatal("media byte budget not enforced")
	}
	for i := 0; i < maxMediaFrames; i++ {
		if q.push(websocket.BinaryMessage, []byte{1}) != nil {
			t.Fatal("early overflow")
		}
	}
	if q.push(websocket.BinaryMessage, []byte{1}) != ErrSlowConsumer {
		t.Fatal("media frame budget not enforced")
	}
	q = newOutputQueue()
	_ = q.push(websocket.BinaryMessage, []byte{1})
	q.media[0].at = time.Now().Add(-2 * time.Second)
	if _, _, err := q.pop(); err != ErrSlowConsumer {
		t.Fatal("stale WebM byte budget not enforced")
	}
}
func TestRTPClockUnwrapsBeyondHalfPeriodAndUsesSenderReports(t *testing.T) {
	origin := time.Unix(1700000000, 0)
	clock := &rtpClock{origin: origin, rate: 90000}
	timestamp := uint32(0xfffffff0)
	clock.anchor(timestamp, origin)
	var pts int64
	for range 10000 {
		timestamp += 900000
		var err error
		pts, err = clock.timestamp(timestamp)
		if err != nil {
			t.Fatal(err)
		}
	}
	if pts != 100000000 {
		t.Fatalf("incremental RTP unwrap failed: %d", pts)
	}
	clock = &rtpClock{origin: origin, rate: 90000}
	clock.anchor(1000, origin)
	ntpSeconds := uint64(origin.Unix() + 2208988800)
	clock.senderReport(&rtcp.SenderReport{RTPTime: 1000, NTPTime: ntpSeconds<<32 | uint64(1<<32/10)}) // common SR is +100ms
	pts, err := clock.timestamp(10000)
	if err != nil || pts <= 100 || pts > 110 {
		t.Fatalf("SR wasn't slewed with monotonic bound: %d %v", pts, err)
	}
}
