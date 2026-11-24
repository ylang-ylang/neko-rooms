package aggregate

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pion/rtcp"
)

// rtpClock unwraps incremental timestamps (including wrap), not a signed delta
// from the first RTP timestamp, which reverses after half of the 32-bit clock.
// Startup is arrival-anchored as in the proved remux. Sender Reports then slew
// toward a shared NTP timeline without reversing emitted sample timestamps.
// SR describes the native sender's RTP clock, not original encoder capture PTS.
type rtpClock struct {
	mu       sync.Mutex
	origin   time.Time
	rate     uint32
	started  bool
	previous uint32
	ticks    int64
	anchorMS int64
	offsetMS int64
	lastPTS  int64
	report   *rtcp.SenderReport
}

func (c *rtpClock) anchor(timestamp uint32, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return
	}
	c.started = true
	c.previous = timestamp
	c.anchorMS = at.Sub(c.origin).Milliseconds()
	c.lastPTS = c.anchorMS - 1
}
func (c *rtpClock) senderReport(report *rtcp.SenderReport) {
	copy := *report
	c.mu.Lock()
	defer c.mu.Unlock()
	c.report = &copy
}
func (c *rtpClock) timestamp(timestamp uint32) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started || c.rate == 0 {
		return 0, errors.New("uninitialized RTP clock")
	}
	delta := int64(int32(timestamp - c.previous))
	if delta < 0 {
		return 0, errors.New("reconstructed RTP sample timestamp reversed")
	}
	c.previous = timestamp
	c.ticks += delta
	nominal := c.anchorMS + c.ticks*1000/int64(c.rate)
	if c.report != nil {
		ntp := c.report.NTPTime
		// NTP epoch is 1900, Unix epoch 1970; the fractional part is 2^-32 s.
		wallMS := (int64(ntp>>32)-2208988800)*1000 + int64((ntp&0xffffffff)*1000>>32)
		target := wallMS - c.origin.UnixMilli() + int64(int32(timestamp-c.report.RTPTime))*1000/int64(c.rate)
		correction := target - nominal - c.offsetMS
		if correction > 1000 || correction < -1000 {
			return 0, fmt.Errorf("RTP Sender Report clock diverged by over one second: rate=%d target_ms=%d nominal_ms=%d offset_ms=%d correction_ms=%d sr_packets=%d sr_octets=%d sample_rtp=%d sr_rtp=%d", c.rate, target, nominal, c.offsetMS, correction, c.report.PacketCount, c.report.OctetCount, timestamp, c.report.RTPTime)
		}
		// At most 10% of this sample interval: bounded drift correction never
		// turns a normally increasing RTP clock into backwards WebM PTS.
		step := delta * 100 / int64(c.rate)
		if delta > 0 && step < 1 {
			step = 1
		}
		if correction > step {
			correction = step
		}
		if correction < -step {
			correction = -step
		}
		c.offsetMS += correction
	}
	pts := nominal + c.offsetMS
	if pts < c.lastPTS {
		return 0, errors.New("common media clock reversed")
	}
	c.lastPTS = pts
	return pts, nil
}
