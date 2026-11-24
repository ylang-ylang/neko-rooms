package aggregate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/m1k1o/neko-rooms/internal/types"
)

type fixtureEvents struct {
	events chan types.RoomEvent
	errs   chan error
}

func (f *fixtureEvents) Events(ctx context.Context) (<-chan types.RoomEvent, <-chan error) {
	go func() { <-ctx.Done(); close(f.errs) }()
	return f.events, f.errs
}

func retryClose(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for range 10 {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			closeErr, ok := err.(*websocket.CloseError)
			if !ok || closeErr.Code != 1013 {
				t.Fatalf("not a retryable 1013 close: %v", err)
			}
			return
		}
		if kind == websocket.TextMessage {
			var message map[string]any
			_ = json.Unmarshal(data, &message)
			if message["type"] == "error" {
				t.Fatalf("retryable teardown would erase the room token: %s", data)
			}
		} else {
			t.Fatal("media was queued after teardown")
		}
	}
	t.Fatal("viewer didn't close")
}
func TestResizeAndSlowQueueUseRetryableCloseWithoutFatalError(t *testing.T) {
	for _, reason := range []string{"resize", "slow queue"} {
		t.Run(reason, func(t *testing.T) {
			h := newHarness(t, nil)
			conn := h.connect(t, tokenA)
			jsonType(t, conn, "state")
			native := receive(t, h.native.conns)
			peer := receive(t, h.factory.peers)
			receive(t, native.incoming)
			if err := peer.cb.Ready(MediaInfo{MIME: fixtureMIME}); err != nil {
				t.Fatal(err)
			}
			jsonType(t, conn, "hello")
			if reason == "resize" {
				if err := native.send("screen/updated", map[string]int{"width": 1920, "height": 1080}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := peer.cb.Media([]byte(strings.Repeat("x", maxMediaBytes+1))); err != ErrSlowConsumer {
					t.Fatal("slow queue not rejected")
				}
			}
			retryClose(t, conn)
			receive(t, peer.closed)
		})
	}
}
func TestRoomEventSubscriptionCancelsActualViewer(t *testing.T) {
	h := newHarness(t, nil)
	source := &fixtureEvents{events: make(chan types.RoomEvent), errs: make(chan error)}
	h.gateway.WatchRooms(source)
	conn := h.connect(t, tokenA)
	jsonType(t, conn, "state")
	peer := receive(t, h.factory.peers)
	source.events <- types.RoomEvent{ID: "aaaaaaaaaaaa", Action: types.RoomEventDestroyed}
	expectClosed(t, conn)
	receive(t, peer.closed)
}
