// Package webm packages complete VP8/Opus samples as WebM for browser MSE.
// Ported from the separately validated native Neko private-Pion media proof.
// It performs no decode or encode. The caller owns media authorization, clock
// synchronization, epoch changes, backpressure, and track-stall cancellation.
package webm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/at-wat/ebml-go"
	"github.com/at-wat/ebml-go/webm"
)

const (
	Video           = 1
	Audio           = 2
	maxPendingBytes = 8 << 20
)

type Sample struct {
	Track    int
	PTS      int64  // milliseconds on one common, monotonic timeline; not raw RTP ticks
	Data     []byte // complete encoded access unit; no RTP payload headers
	Keyframe bool
}

type Chunk struct {
	Init           bool
	RandomAccess   bool
	StartMS, EndMS int64
	Data           []byte
}

// Muxer has one owner goroutine. Push retains Data until the sample is emitted.
// A fatal error requires a new epoch/init/keyframe, not arbitrary byte dropping.
type Muxer struct {
	started       bool
	audioSeen     bool
	preAudio      []Sample
	origin, start int64
	latest        [3]int64
	pending       []Sample
	channels      uint16
	width, height int
	last          [3]int64
	emit          func(Chunk) error
}

func New(channels uint16, emit func(Chunk) error) *Muxer {
	return &Muxer{channels: channels, emit: emit, latest: [3]int64{-1, -1, -1}, last: [3]int64{-1, -1, -1}}
}

// VP8Dimensions checks a genuine VP8 keyframe header and extracts coded size.
func VP8Dimensions(b []byte) (int, int, error) {
	if len(b) < 10 || b[0]&1 != 0 || !bytes.Equal(b[3:6], []byte{0x9d, 0x01, 0x2a}) {
		return 0, 0, errors.New("not a complete VP8 keyframe")
	}
	w, h := int(binary.LittleEndian.Uint16(b[6:8])&0x3fff), int(binary.LittleEndian.Uint16(b[8:10])&0x3fff)
	if w == 0 || h == 0 {
		return 0, 0, errors.New("zero VP8 dimensions")
	}
	return w, h, nil
}

func Init(width, height int, channels uint16) ([]byte, error) {
	if channels < 1 || channels > 2 {
		return nil, errors.New("WebM remux supports mono/stereo Opus")
	}
	// Live RTP remux starts mid-encoder. Original pre-skip is not signaled in RTP.
	// Policy: zero pre-skip and CodecDelay=0, retaining standard 80ms preroll.
	opus := make([]byte, 19)
	copy(opus, "OpusHead")
	opus[8] = 1
	opus[9] = byte(channels)
	binary.LittleEndian.PutUint32(opus[12:16], 48000)
	header := struct {
		Header  webm.EBMLHeader `ebml:"EBML"`
		Segment struct {
			Info   webm.Info   `ebml:"Info"`
			Tracks webm.Tracks `ebml:"Tracks"`
		} `ebml:"Segment,size=unknown"`
	}{Header: *webm.DefaultEBMLHeader}
	header.Segment.Info = webm.Info{TimecodeScale: 1000000, MuxingApp: "Neko Rooms HTTP aggregation", WritingApp: "ebml-go"}
	header.Segment.Tracks = webm.Tracks{TrackEntry: []webm.TrackEntry{
		{TrackNumber: 1, TrackUID: 1, TrackType: 1, CodecID: "V_VP8", Video: &webm.Video{PixelWidth: uint64(width), PixelHeight: uint64(height)}},
		{TrackNumber: 2, TrackUID: 2, TrackType: 2, CodecID: "A_OPUS", CodecPrivate: opus, SeekPreRoll: 80000000, Audio: &webm.Audio{SamplingFrequency: 48000, Channels: uint64(channels)}},
	}}
	var out bytes.Buffer
	if err := ebml.Marshal(&header, &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (m *Muxer) Push(s Sample) error {
	if s.Track != Video && s.Track != Audio {
		return errors.New("unknown track")
	}
	if len(s.Data) == 0 || len(s.Data) > maxPendingBytes {
		return errors.New("empty or oversized access unit")
	}
	if !m.started {
		if s.Track == Audio {
			m.audioSeen = true
			m.preAudio = append(m.preAudio, s)
			if len(m.preAudio) > 64 {
				m.preAudio = m.preAudio[len(m.preAudio)-64:]
			}
			if sampleBytes(m.preAudio) > maxPendingBytes {
				return errors.New("bounded pre-audio queue exceeded")
			}
			return nil
		}
		if !s.Keyframe || !m.audioSeen {
			return nil
		}
		w, h, err := VP8Dimensions(s.Data)
		if err != nil {
			return err
		}
		m.width, m.height = w, h
		init, err := Init(w, h, m.channels)
		if err != nil {
			return err
		}
		m.origin = s.PTS
		m.start = 0
		m.started = true
		for _, a := range m.preAudio {
			a.PTS -= m.origin
			if a.PTS >= 0 {
				m.pending = append(m.pending, a)
				m.latest[Audio] = a.PTS
				m.last[Audio] = a.PTS
			}
		}
		m.preAudio = nil
		if err = m.emit(Chunk{Init: true, Data: init}); err != nil {
			return err
		}
	}
	if s.Track == Video && s.Keyframe {
		w, h, err := VP8Dimensions(s.Data)
		if err != nil {
			return err
		}
		if w != m.width || h != m.height {
			return errors.New("VP8 dimensions changed: start a new media epoch")
		}
	}
	s.PTS -= m.origin
	if s.PTS < 0 {
		return nil
	} // discard audio preceding the initial video random access
	if s.PTS < m.last[s.Track] {
		return fmt.Errorf("track %d timestamp reversed: %d < %d", s.Track, s.PTS, m.last[s.Track])
	}
	m.last[s.Track] = s.PTS
	m.latest[s.Track] = s.PTS
	m.pending = append(m.pending, s)
	if len(m.pending) > 512 || sampleBytes(m.pending) > maxPendingBytes {
		return errors.New("bounded mux sample queue exceeded")
	}
	if m.latest[Video]-m.latest[Audio] > 1000 || m.latest[Audio]-m.latest[Video] > 1000 {
		return errors.New("track timing diverged or a track stalled")
	}
	for {
		boundary := m.start + 100
		first := [3]int64{-1, -1, -1}
		for _, f := range m.pending {
			if first[f.Track] < 0 {
				first[f.Track] = f.PTS
			}
		}
		if first[Video] < 0 || first[Audio] < 0 {
			break
		}
		// Extend a missing-track window through its first actual sample rather
		// than emitting an invalid one-track Cluster with a two-track init.
		for i := 1; i <= 2; i++ {
			if first[i] >= boundary {
				boundary = first[i] + 1
			}
		}
		if m.latest[Video] < boundary || m.latest[Audio] < boundary {
			break
		}
		var selected, rest []Sample
		for _, f := range m.pending {
			if f.PTS < boundary {
				selected = append(selected, f)
			} else {
				rest = append(rest, f)
			}
		}
		if err := m.cluster(selected, m.start, boundary); err != nil {
			return err
		}
		m.pending = rest
		m.start = boundary
	}
	return nil
}

func sampleBytes(samples []Sample) int {
	total := 0
	for _, s := range samples {
		total += len(s.Data)
	}
	return total
}

func (m *Muxer) cluster(samples []Sample, start, end int64) error {
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].PTS < samples[j].PTS })
	tracks := 0
	random := false
	videoSeen := false
	blocks := make([]ebml.Block, 0, len(samples))
	for _, s := range samples {
		tracks |= 1 << s.Track
		if s.Track == Video && !videoSeen {
			random = s.Keyframe
			videoSeen = true
		}
		blocks = append(blocks, ebml.Block{TrackNumber: uint64(s.Track), Timecode: int16(s.PTS - start), Keyframe: s.Keyframe, Data: [][]byte{s.Data}})
	}
	if tracks != 1<<Video|1<<Audio {
		return fmt.Errorf("MSE cluster [%d,%d) missing one track (mask %d)", start, end, tracks)
	}
	value := struct {
		Cluster webm.Cluster `ebml:"Cluster"`
	}{webm.Cluster{Timecode: uint64(start), SimpleBlock: blocks}}
	var out bytes.Buffer
	if err := ebml.Marshal(&value, &out); err != nil {
		return err
	}
	return m.emit(Chunk{RandomAccess: random, StartMS: start, EndMS: end, Data: out.Bytes()})
}
