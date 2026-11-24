package aggregate

import (
	"strings"
	"testing"
	"time"

	"github.com/pion/rtcp"
)

func TestRTPClockIgnoresEmptySenderReportBeforeColdCapture(t *testing.T) {
	origin := time.Unix(1795527091, 0)
	ntp := func(ms uint64) uint64 {
		return uint64(origin.Unix()+2208988800+int64(ms/1000))<<32 | (ms%1000)<<32/1000
	}
	// Exact RTP values from the real fullHD30/native-default cold-start
	// failure: the native one-second SR timer fired before any audio RTP.
	const firstRTP = uint32(3868373476)
	empty := &rtcp.SenderReport{RTPTime: 923864645, NTPTime: ntp(1000)}
	for _, beforePacket := range []bool{true, false} {
		clock := &rtpClock{origin: origin, rate: 48000}
		if beforePacket {
			clock.senderReport(empty)
		}
		clock.anchor(firstRTP, origin.Add(1700*time.Millisecond))
		if !beforePacket {
			clock.senderReport(empty)
		}
		pts, err := clock.timestamp(firstRTP)
		if err != nil || pts != 1700 {
			t.Fatalf("empty startup SR poisoned arrival anchor: pts=%d err=%v", pts, err)
		}
		if clock.report != nil {
			t.Fatal("empty report became an RTP/NTP mapping")
		}

		valid := &rtcp.SenderReport{RTPTime: firstRTP + 4800, NTPTime: ntp(1800), PacketCount: 6, OctetCount: 600}
		clock.senderReport(valid)
		pts, err = clock.timestamp(firstRTP + 4800)
		if err != nil || pts < 1799 || pts > 1800 {
			t.Fatalf("valid media-backed SR wasn't used: pts=%d err=%v", pts, err)
		}
		clock.senderReport(empty)
		if clock.report.PacketCount != valid.PacketCount {
			t.Fatal("late empty report replaced a valid clock mapping")
		}
		pts, err = clock.timestamp(firstRTP + 9600)
		if err != nil || pts < 1899 || pts > 1900 {
			t.Fatalf("mapping didn't survive stale empty report: pts=%d err=%v", pts, err)
		}
	}
	// Do not hide genuine media-backed clock divergence by loosening its bound.
	clock := &rtpClock{origin: origin, rate: 48000}
	clock.anchor(firstRTP, origin.Add(1700*time.Millisecond))
	bad := *empty
	bad.PacketCount = 1
	bad.OctetCount = 100
	clock.senderReport(&bad)
	if _, err := clock.timestamp(firstRTP); err == nil || !strings.Contains(err.Error(), "diverged by over one second") {
		t.Fatalf("media-backed divergence guard lost: %v", err)
	}
}
