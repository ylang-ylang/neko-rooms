package webm

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/at-wat/ebml-go"
	ebmlwebm "github.com/at-wat/ebml-go/webm"
)

func header(width, height uint16) []byte {
	data := []byte{0x10, 0, 0, 0x9d, 0x01, 0x2a, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(data[6:8], width)
	binary.LittleEndian.PutUint16(data[8:10], height)
	return data
}
func TestMuxStartsWithBothTracksRealKeyframeAndInterleavedFiniteClusters(t *testing.T) {
	var chunks []Chunk
	m := New(2, func(chunk Chunk) error { chunks = append(chunks, chunk); return nil })
	if m.Push(Sample{Track: Video, PTS: 0, Data: header(640, 360), Keyframe: true}) != nil || len(chunks) != 0 {
		t.Fatal("mux emitted before audio available")
	}
	for pts := int64(0); pts <= 500; pts += 20 {
		if err := m.Push(Sample{Track: Audio, PTS: pts, Data: []byte{0xf8, 0xff, 0xfe}, Keyframe: true}); err != nil {
			t.Fatal(err)
		}
		if pts%100 == 0 {
			if err := m.Push(Sample{Track: Video, PTS: pts, Data: header(640, 360), Keyframe: true}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(chunks) < 3 || !chunks[0].Init || !chunks[1].RandomAccess {
		t.Fatal("missing init/keyframe/Clusters")
	}
	var init struct {
		Header  ebmlwebm.EBMLHeader `ebml:"EBML"`
		Segment struct {
			Info   ebmlwebm.Info   `ebml:"Info"`
			Tracks ebmlwebm.Tracks `ebml:"Tracks"`
		} `ebml:"Segment"`
	}
	if err := ebml.Unmarshal(bytes.NewReader(chunks[0].Data), &init); err != nil {
		t.Fatal(err)
	}
	tracks := init.Segment.Tracks.TrackEntry
	if init.Header.DocType != "webm" || init.Segment.Info.TimecodeScale != 1000000 || len(tracks) != 2 || tracks[0].CodecID != "V_VP8" || tracks[1].CodecID != "A_OPUS" || tracks[0].Video.PixelWidth != 640 || tracks[0].Video.PixelHeight != 360 || string(tracks[1].CodecPrivate[:8]) != "OpusHead" || tracks[1].CodecDelay != 0 || tracks[1].SeekPreRoll != 80000000 {
		t.Fatal("wrong VP8/Opus WebM init")
	}
	for _, chunk := range chunks[1:] {
		var value struct {
			Cluster ebmlwebm.Cluster `ebml:"Cluster"`
		}
		if err := ebml.Unmarshal(bytes.NewReader(chunk.Data), &value); err != nil {
			t.Fatal(err)
		}
		mask := uint64(0)
		last := int16(-1)
		for _, block := range value.Cluster.SimpleBlock {
			mask |= 1 << block.TrackNumber
			if block.Timecode < last {
				t.Fatal("Cluster block PTS reversed")
			}
			last = block.Timecode
		}
		if mask != 1<<Video|1<<Audio {
			t.Fatal("two-track MSE Cluster lacks a track")
		}
	}
}
func TestMuxRejectsInvalidKeyframeResizeAndTimestampReversal(t *testing.T) {
	if _, _, err := VP8Dimensions([]byte{0}); err == nil {
		t.Fatal("not a keyframe")
	}
	makeMux := func() *Muxer {
		m := New(2, func(Chunk) error { return nil })
		_ = m.Push(Sample{Track: Audio, PTS: 0, Data: []byte{1}})
		_ = m.Push(Sample{Track: Video, PTS: 0, Data: header(640, 360), Keyframe: true})
		return m
	}
	if makeMux().Push(Sample{Track: Video, PTS: 100, Data: header(1280, 720), Keyframe: true}) == nil {
		t.Fatal("resize appended into old WebM epoch")
	}
	m := makeMux()
	_ = m.Push(Sample{Track: Audio, PTS: 20, Data: []byte{1}})
	if m.Push(Sample{Track: Audio, PTS: 10, Data: []byte{1}}) == nil {
		t.Fatal("timestamp reversal accepted")
	}
}
