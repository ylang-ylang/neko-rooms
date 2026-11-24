package aggregate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	webm "github.com/m1k1o/neko-rooms/internal/aggregate/webm"
)

const webmMIME = `video/webm; codecs="vp8,opus"`
const maxSampleQueueBytes = 8 << 20

// PionFactory receives native VP8/Opus without decoding/re-encoding. It never
// reads credentials, creates native sessions, uses TURN/STUN, or owns native WS.
type PionFactory struct{}

func (PionFactory) NewPeer(parent context.Context, cb PeerCallbacks) (Peer, error) {
	engine := &webrtc.MediaEngine{}
	if err := engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}}}, PayloadType: 96}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	if err := engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}, PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	settings := webrtc.SettingEngine{}
	settings.SetIPFilter(privateIP)
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	p := &privatePeer{pc: pc, ctx: ctx, cancel: cancel, cb: cb, samples: make(chan trackSample, 128), origin: time.Now(), done: make(chan struct{})}
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil || p.ctx.Err() != nil {
			return
		}
		if err := cb.Signal("signal/candidate", candidate.ToJSON()); err != nil {
			p.fail(err)
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected {
			p.fail(errors.New("private peer disconnected"))
		}
	})
	pc.OnDataChannel(p.onDataChannel)
	pc.OnTrack(p.onTrack)
	p.wg.Add(1)
	go func() { defer p.wg.Done(); p.mux() }()
	return p, nil
}

type trackSample struct {
	webm.Sample
	dropped  bool
	channels uint16
}

type privatePeer struct {
	pc          *webrtc.PeerConnection
	ctx         context.Context
	cancel      context.CancelFunc
	cb          PeerCallbacks
	origin      time.Time
	mu          sync.Mutex
	closed      bool
	tracks      [3]bool
	wg          sync.WaitGroup
	closeOnce   sync.Once
	done        chan struct{}
	failOnce    sync.Once
	signalMu    sync.Mutex
	remote      bool
	candidates  []webrtc.ICECandidateInit
	dataMu      sync.Mutex
	data        *webrtc.DataChannel
	samples     chan trackSample
	queuedBytes atomic.Int64
}

func privateIP(ip net.IP) bool {
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast())
}
func privateCandidate(raw string) error {
	candidate, err := ice.UnmarshalCandidate(strings.TrimPrefix(raw, "candidate:"))
	if err != nil || candidate.Type() != ice.CandidateTypeHost || !privateIP(net.ParseIP(candidate.Address())) {
		return errors.New("native ICE candidate is not a private host candidate")
	}
	return nil
}

func (p *privatePeer) fail(err error) {
	if p.ctx.Err() != nil {
		return
	}
	p.failOnce.Do(func() { p.cancel(); p.cb.Fail(err) })
}

func (p *privatePeer) HandleSignal(event string, payload json.RawMessage) error {
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	switch event {
	case "signal/candidate":
		var candidate webrtc.ICECandidateInit
		if err := decodeJSON(payload, &candidate); err != nil {
			return err
		}
		if candidate.Candidate != "" {
			if err := privateCandidate(candidate.Candidate); err != nil {
				return err
			}
		}
		if !p.remote {
			if len(p.candidates) >= 256 {
				return errors.New("private ICE candidate queue exceeded")
			}
			p.candidates = append(p.candidates, candidate)
			return nil
		}
		return p.pc.AddICECandidate(candidate)
	case "signal/provide", "signal/offer", "signal/restart":
		var offer struct {
			SDP string `json:"sdp"`
		}
		if err := decodeJSON(payload, &offer); err != nil {
			return err
		}
		if len(offer.SDP) == 0 || len(offer.SDP) > maxNativeJSON {
			return errors.New("invalid native offer")
		}
		for _, line := range strings.Split(offer.SDP, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "a=candidate:") {
				if err := privateCandidate(strings.TrimPrefix(line, "a=")); err != nil {
					return err
				}
			}
		}
		// Ignore native iceservers. Configuration{} above deliberately has no
		// STUN/TURN, public NAT rewriting, or browser-facing ICE credentials.
		if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
			return err
		}
		p.remote = true
		for _, candidate := range p.candidates {
			if err := p.pc.AddICECandidate(candidate); err != nil {
				return err
			}
		}
		p.candidates = nil
		answer, err := p.pc.CreateAnswer(nil)
		if err != nil {
			return err
		}
		if err = p.pc.SetLocalDescription(answer); err != nil {
			return err
		}
		return p.cb.Signal("signal/answer", map[string]string{"sdp": answer.SDP})
	default:
		return fmt.Errorf("unsupported private signaling event %q", event)
	}
}

func (p *privatePeer) onDataChannel(data *webrtc.DataChannel) {
	if data.Label() != "data" || !data.Ordered() || data.MaxPacketLifeTime() != nil || data.MaxRetransmits() != nil {
		_ = data.Close()
		return
	}
	p.dataMu.Lock()
	if p.data != nil || p.ctx.Err() != nil {
		p.dataMu.Unlock()
		_ = data.Close()
		p.fail(errors.New("private input channel replaced"))
		return
	}
	p.data = data
	p.dataMu.Unlock()
	data.OnMessage(func(message webrtc.DataChannelMessage) {
		if !message.IsString && p.ctx.Err() == nil {
			p.cb.Data(message.Data)
		}
	})
	data.OnClose(func() { p.fail(errors.New("private input channel closed")) })
	data.OnError(func(err error) { p.fail(err) })
}

func (p *privatePeer) SendData(data []byte) error {
	p.dataMu.Lock()
	defer p.dataMu.Unlock()
	if p.ctx.Err() != nil || p.data == nil || p.data.ReadyState() != webrtc.DataChannelStateOpen {
		return ErrInputUnavailable
	}
	if p.data.BufferedAmount() > 4096 {
		p.fail(errors.New("private input channel congested"))
		return ErrInputUnavailable
	}
	// No Manager-side input queue, retries or reconnect replay. Neko's native
	// ordered channel checks IsHost at DELIVERY, unlike its WS input handlers.
	if err := p.data.Send(data); err != nil {
		// A failed keyup cannot leave native held keys behind while local
		// tracking is cleared. Reset the whole viewer through normal WS close.
		p.fail(err)
		return ErrInputUnavailable
	}
	return nil
}

func (p *privatePeer) onTrack(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	codec := track.Codec()
	kind := 0
	switch strings.ToLower(codec.MimeType) {
	case "video/vp8":
		if codec.ClockRate == 90000 {
			kind = webm.Video
		}
	case "audio/opus":
		if codec.ClockRate == 48000 && codec.Channels >= 1 && codec.Channels <= 2 {
			kind = webm.Audio
		}
	}
	if kind == 0 {
		p.fail(errors.New("native media must be VP8/90000 and Opus/48000 mono/stereo"))
		return
	}
	p.mu.Lock()
	if p.closed || p.ctx.Err() != nil {
		p.mu.Unlock()
		return
	}
	if p.tracks[kind] {
		p.mu.Unlock()
		p.fail(errors.New("native media track replaced; new epoch required"))
		return
	}
	p.tracks[kind] = true
	p.wg.Add(2)
	p.mu.Unlock()
	clock := &rtpClock{origin: p.origin, rate: codec.ClockRate}
	go func() { defer p.wg.Done(); p.receive(track, kind, codec.Channels, clock) }()
	go func() {
		defer p.wg.Done()
		for p.ctx.Err() == nil {
			packets, _, err := receiver.ReadRTCP()
			if err != nil {
				return
			}
			for _, packet := range packets {
				if report, ok := packet.(*rtcp.SenderReport); ok && report.SSRC == uint32(track.SSRC()) {
					clock.senderReport(report)
				}
			}
		}
	}()
}

func (p *privatePeer) receive(track *webrtc.TrackRemote, kind int, channels uint16, clock *rtpClock) {
	var builder *samplebuilder.SampleBuilder
	if kind == webm.Video {
		// Genuine desktop keyframes span far more than 32 RTP packets. This
		// proved 512-packet/100ms bound must not be mistaken for a frame count.
		builder = samplebuilder.New(512, &codecs.VP8Packet{}, 90000, samplebuilder.WithMaxTimeDelay(100*time.Millisecond))
	} else {
		builder = samplebuilder.New(16, &codecs.OpusPacket{}, 48000, samplebuilder.WithMaxTimeDelay(100*time.Millisecond))
	}
	for p.ctx.Err() == nil {
		packet, _, err := track.ReadRTP()
		if err != nil {
			p.fail(errors.New("native media track ended"))
			return
		}
		clock.anchor(packet.Timestamp, time.Now())
		builder.Push(packet)
		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			pts, err := clock.timestamp(sample.PacketTimestamp)
			if err != nil {
				p.fail(err)
				return
			}
			key := kind == webm.Audio || (len(sample.Data) > 0 && sample.Data[0]&1 == 0)
			if kind == webm.Video && key {
				if _, _, err := webm.VP8Dimensions(sample.Data); err != nil {
					p.fail(err)
					return
				}
			}
			frame := trackSample{Sample: webm.Sample{Track: kind, PTS: pts, Data: sample.Data, Keyframe: key}, dropped: sample.PrevDroppedPackets > 0, channels: channels}
			if p.queuedBytes.Add(int64(len(sample.Data))) > maxSampleQueueBytes {
				p.queuedBytes.Add(-int64(len(sample.Data)))
				p.fail(errors.New("private media sample byte budget exceeded"))
				return
			}
			select {
			case p.samples <- frame:
			case <-p.ctx.Done():
				p.queuedBytes.Add(-int64(len(sample.Data)))
				return
			default:
				p.queuedBytes.Add(-int64(len(sample.Data)))
				p.fail(errors.New("private media sample queue exceeded"))
				return
			}
		}
	}
}

func (p *privatePeer) mux() {
	var mux *webm.Muxer
	var last [3]time.Time
	ready := false
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	// Audio may arrive after the first video frames. Keep only a bounded
	// startup sample window; the mux still waits for audio AND a real keyframe.
	var startup []trackSample
	for {
		select {
		case <-p.ctx.Done():
			return
		case frame := <-p.samples:
			p.queuedBytes.Add(-int64(len(frame.Data)))
			last[frame.Track] = time.Now()
			if mux == nil {
				if frame.Track != webm.Audio {
					startup = append(startup, frame)
					if len(startup) > 64 {
						startup = startup[len(startup)-64:]
					}
					if startupBytes(startup) > maxSampleQueueBytes {
						p.fail(errors.New("private media startup byte budget exceeded"))
						return
					}
					continue
				}
				mux = webm.New(frame.channels, func(chunk webm.Chunk) error {
					if chunk.Init {
						ready = true
						if err := p.cb.Ready(MediaInfo{MIME: webmMIME}); err != nil {
							return err
						}
					}
					return p.cb.Media(chunk.Data)
				})
				if err := mux.Push(frame.Sample); err != nil {
					p.fail(err)
					return
				}
				for _, pending := range startup {
					if err := mux.Push(pending.Sample); err != nil {
						p.fail(err)
						return
					}
				}
				startup = nil
				continue
			}
			if ready && frame.Track == webm.Video && frame.dropped {
				p.fail(errors.New("VP8 packet loss: new keyframe epoch required"))
				return
			}
			if err := mux.Push(frame.Sample); err != nil {
				p.fail(err)
				return
			}
		case <-ticker.C:
			if ready || time.Since(p.origin) > 5*time.Second {
				for kind := 1; kind <= 2; kind++ {
					if last[kind].IsZero() || time.Since(last[kind]) > 2*time.Second {
						p.fail(errors.New("native media track stalled"))
						return
					}
				}
			}
			if !ready && time.Since(p.origin) > 10*time.Second {
				p.fail(errors.New("native VP8 keyframe/audio startup timed out"))
				return
			}
		}
	}
}

func startupBytes(samples []trackSample) int {
	total := 0
	for _, sample := range samples {
		total += len(sample.Data)
	}
	return total
}

func (p *privatePeer) Close() error {
	var err error
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.cancel()
		p.mu.Unlock()
		err = p.pc.Close() // unblocks TrackRemote.ReadRTP and receiver.ReadRTCP
		p.wg.Wait()
		close(p.done)
	})
	<-p.done
	return err
}
