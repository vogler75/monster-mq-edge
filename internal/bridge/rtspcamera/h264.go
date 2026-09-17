package rtspcamera

import (
	"bytes"
	"errors"
	"fmt"
	"sync/atomic"

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
	var ssrc uint32
	haveSSRC := false
	var buf bytes.Buffer
	report := func(err error) {
		c.mu.Lock()
		changed := c.lastError != err.Error()
		c.lastError = err.Error()
		c.latestFrame = nil
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
		frames, err := dec.Decode(au)
		if err != nil {
			report(err)
			return
		}
		for _, frame := range frames {
			buf.Reset()
			if err := frame.WriteJPEG(&buf, 85); err != nil {
				report(err)
				return
			}
			atomic.AddUint64(&c.framesReceived, 1)
			c.setLatestFrame(buf.Bytes())
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
