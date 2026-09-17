package rtspcamera

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"monstermq.io/edge/pkg/h264"
)

func (c *Connector) h264PacketHandler(f *format.H264, fatal chan<- error) (func(*rtp.Packet), error) {
	if f.PacketizationMode != 0 && f.PacketizationMode != 1 {
		return nil, fmt.Errorf("%w: H.264 packetization mode %d", h264.ErrUnsupported, f.PacketizationMode)
	}
	dec := h264.NewDecoder(h264.Config{})
	var params [][]byte
	sps, pps := f.SafeParams()
	if len(sps) > 0 {
		params = append(params, sps)
	}
	if len(pps) > 0 {
		params = append(params, pps)
	}
	if _, err := dec.Decode(params); err != nil {
		return nil, fmt.Errorf("H.264 SDP parameter sets: %w", err)
	}
	dep := &h264.Depacketizer{}
	keyframesOnly := c.cfg.H264DecodeMode == H264DecodeKeyframesOnly
	var lastKeyframe time.Time
	c.logger.Info("H.264 capture configured", "decodeMode", c.cfg.H264DecodeMode, "intervalMs", c.cfg.IntervalMs)
	var ssrc uint32
	haveSSRC := false
	report := func(err error) {
		lastKeyframe = time.Time{}
		c.mu.Lock()
		changed := c.lastError != err.Error()
		c.lastError = err.Error()
		c.latestFrame = nil
		c.latestDecoded = nil
		c.mu.Unlock()
		if changed {
			c.publishStatus()
		}
		if errors.Is(err, h264.ErrUnsupported) {
			select {
			case fatal <- err:
			default:
			}
		}
	}
	return func(pkt *rtp.Packet) {
		if haveSSRC && ssrc != pkt.SSRC {
			dep.Reset()
			dec.Discontinuity()
			lastKeyframe = time.Time{}
			c.mu.Lock()
			c.latestFrame, c.latestDecoded = nil, nil
			c.mu.Unlock()
		}
		haveSSRC, ssrc = true, pkt.SSRC
		au, err := dep.Push(h264.RTPPacket{SequenceNumber: pkt.SequenceNumber, Timestamp: pkt.Timestamp, Marker: pkt.Marker, Payload: pkt.Payload})
		if err != nil {
			dec.Discontinuity()
			report(err)
			return
		}
		if len(au) == 0 {
			return
		}
		var selectedAt time.Time
		if keyframesOnly {
			idr := false
			var parameterSets [][]byte
			for _, nal := range au {
				switch nal[0] & 31 {
				case 5:
					idr = true
				case 7, 8:
					parameterSets = append(parameterSets, nal)
				}
			}
			selectedAt = time.Now()
			if !idr || !lastKeyframe.IsZero() && selectedAt.Sub(lastKeyframe).Milliseconds() < int64(c.cfg.IntervalMs) {
				// Parameter sets can change in an access unit whose picture is
				// skipped. Retain those changes for the next selected IDR.
				if _, err := dec.Decode(parameterSets); err != nil {
					report(err)
				}
				return
			}
			dec.Discontinuity()
		}
		frames, err := dec.Decode(au)
		if err != nil {
			report(err)
			return
		}
		if keyframesOnly {
			// Each selected IDR is an independent sequence. B-frame streams
			// may otherwise delay even this picture for display reordering.
			frames = append(frames, dec.Flush()...)
			dec.Discontinuity()
			lastKeyframe = selectedAt
		}
		for _, frame := range frames {
			atomic.AddUint64(&c.framesReceived, 1)
			c.setLatestDecoded(frame)
			c.mu.Lock()
			hadError := c.lastError != ""
			c.lastError = ""
			c.mu.Unlock()
			if hadError {
				c.publishStatus()
			}
		}
	}, nil
}
