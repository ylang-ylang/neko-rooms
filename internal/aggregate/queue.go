package aggregate

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxMediaBytes    = 4 << 20
	maxMediaFrames   = 64
	maxControlBytes  = 1 << 20
	maxControlFrames = 64
	maxMediaAge      = time.Second
)

type outputFrame struct {
	kind int
	data []byte
	at   time.Time
}

// Control messages have priority over not-yet-written media. Readers and RTP
// callbacks never wait for the public socket. Overflow resets the entire WebM
// epoch by closing the viewer; dropping individual Clusters is not continuity.
type outputQueue struct {
	mu                       sync.Mutex
	control, media           []outputFrame
	controlBytes, mediaBytes int
	closed                   bool
	wake                     chan struct{}
}

func newOutputQueue() *outputQueue { return &outputQueue{wake: make(chan struct{}, 1)} }
func (q *outputQueue) push(kind int, data []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrNotReady
	}
	now := time.Now()
	if kind == websocket.BinaryMessage {
		if len(q.media) >= maxMediaFrames || q.mediaBytes+len(data) > maxMediaBytes || (len(q.media) > 0 && now.Sub(q.media[0].at) > maxMediaAge) {
			return ErrSlowConsumer
		}
		q.media = append(q.media, outputFrame{kind: kind, data: append([]byte(nil), data...), at: now})
		q.mediaBytes += len(data)
	} else {
		if len(q.control) >= maxControlFrames || q.controlBytes+len(data) > maxControlBytes {
			return ErrSlowConsumer
		}
		q.control = append(q.control, outputFrame{kind: kind, data: append([]byte(nil), data...), at: now})
		q.controlBytes += len(data)
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return nil
}
func (q *outputQueue) json(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return q.push(websocket.TextMessage, data)
}
func (q *outputQueue) pop() (outputFrame, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return outputFrame{}, false, ErrNotReady
	}
	if len(q.media) > 0 && time.Since(q.media[0].at) > maxMediaAge {
		return outputFrame{}, false, ErrSlowConsumer
	}
	if len(q.control) > 0 {
		f := q.control[0]
		q.control[0] = outputFrame{}
		q.control = q.control[1:]
		q.controlBytes -= len(f.data)
		return f, true, nil
	}
	if len(q.media) > 0 {
		f := q.media[0]
		q.media[0] = outputFrame{}
		q.media = q.media[1:]
		q.mediaBytes -= len(f.data)
		return f, true, nil
	}
	return outputFrame{}, false, nil
}
func (q *outputQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.control = nil
	q.media = nil
	q.mediaBytes = 0
	q.controlBytes = 0
	select {
	case q.wake <- struct{}{}:
	default:
	}
}
