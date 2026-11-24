// Package aggregate adapts native Neko viewer sessions to the Manager's public
// HTTP/WebSocket viewer. SDP, ICE and data-channel frames never cross that API.
package aggregate

import (
	"context"
	"encoding/json"
	"errors"
)

var (
	ErrNotReady         = errors.New("private media adapter is not ready")
	ErrInputUnavailable = errors.New("private input data channel is not open")
	ErrSlowConsumer     = errors.New("viewer cannot keep up with the live stream")
)

// PeerFactory makes exactly one private media peer for one authenticated native
// session. Native WebSocket authentication and lifetime belong to Gateway.
// Factories must not use a shared administrative session or publish ICE ports.
type PeerFactory interface {
	NewPeer(context.Context, PeerCallbacks) (Peer, error)
}

// Peer methods may be called concurrently with callbacks and Close. SendData
// must fail when native ordered channel "data" is unavailable: never buffer or
// replay input across a channel/peer reconnect, or fall back to native WS input.
type Peer interface {
	HandleSignal(event string, payload json.RawMessage) error
	SendData([]byte) error
	Close() error
}

type MediaInfo struct {
	MIME          string
	Width, Height int
}

// Callbacks must return promptly; Gateway supplies bounded, nonblocking sinks.
// Ready must precede Media, whose chunks are raw WebM (init then Clusters), not
// an additional framing protocol. A new init/epoch requires a new public WS.
// Media must verify actual VP8/Opus codecs and begin at a complete keyframe.
// Data receives native cursor frames. Fail terminates the entire viewer.
type PeerCallbacks struct {
	Signal func(event string, payload any) error
	Ready  func(MediaInfo) error
	Media  func([]byte) error
	Data   func([]byte)
	Fail   func(error)
}
