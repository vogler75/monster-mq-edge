package h264

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrPacketLoss means an access unit was discarded. Invalidate the decoder's
// reference pictures before feeding another access unit after this error.
var ErrPacketLoss = errors.New("h264: lost or out-of-order RTP packet")

// RTPPacket contains the RTP fields used by RFC 6184. Supply packets in sequence
// order after transport-level jitter buffering; sequence numbers wrap at 65536.
type RTPPacket struct {
	SequenceNumber uint16
	Timestamp      uint32
	Marker         bool
	Payload        []byte
}

// Depacketizer assembles RFC 6184 non-interleaved access units (single NAL,
// STAP-A, FU-A). It owns returned NAL bytes. One instance handles one RTP SSRC;
// reset it when the stream or SSRC changes. It is not safe for concurrent use.
type Depacketizer struct {
	MaxAccessUnitBytes           int
	nals                         [][]byte
	fragment                     []byte
	fragmentHeader               byte
	size                         int
	timestamp                    uint32
	sequence                     uint16
	started, sequenced, dropping bool
}

// Reset discards pending packets while preserving the allocation limit.
func (d *Depacketizer) Reset() {
	limit := d.MaxAccessUnitBytes
	*d = Depacketizer{MaxAccessUnitBytes: limit}
}
func (d *Depacketizer) discard() { d.nals = nil; d.fragment = nil; d.size = 0; d.dropping = true }

// Push returns a complete access unit only on its RTP marker packet. A nil
// result with nil error means more packets are needed. Damaged access units are
// discarded in their entirety rather than handing partial slices to the codec.
func (d *Depacketizer) Push(p RTPPacket) (au [][]byte, err error) {
	defer func() {
		if err != nil {
			d.discard()
		}
	}()
	if !d.started || p.Timestamp != d.timestamp {
		incomplete := d.started && !d.dropping && (len(d.nals) != 0 || d.fragment != nil)
		d.nals = nil
		d.fragment = nil
		d.size = 0
		d.dropping = false
		d.started = true
		d.timestamp = p.Timestamp
		if incomplete {
			d.sequence = p.SequenceNumber + 1
			d.sequenced = true
			return nil, ErrPacketLoss
		}
	}
	if d.sequenced && p.SequenceNumber != d.sequence {
		d.sequence = p.SequenceNumber + 1
		return nil, ErrPacketLoss
	}
	d.sequence = p.SequenceNumber + 1
	d.sequenced = true
	if d.dropping {
		return nil, nil
	}
	limit := d.MaxAccessUnitBytes
	if limit <= 0 {
		limit = 16 << 20
	}
	payload := p.Payload
	if len(payload) == 0 || payload[0]&0x80 != 0 {
		return nil, fmt.Errorf("%w: invalid RTP NAL header", ErrMalformed)
	}
	if len(payload) > limit-d.size {
		return nil, fmt.Errorf("%w: RTP access unit exceeds byte limit", ErrMalformed)
	}
	d.size += len(payload)
	typ := payload[0] & 31
	switch {
	case typ >= 1 && typ <= 23:
		if d.fragment != nil {
			return nil, ErrPacketLoss
		}
		d.nals = append(d.nals, append([]byte(nil), payload...))
	case typ == 24:
		if d.fragment != nil {
			return nil, ErrPacketLoss
		}
		payload = payload[1:]
		if len(payload) == 0 {
			return nil, fmt.Errorf("%w: empty STAP-A", ErrMalformed)
		}
		for len(payload) > 0 {
			if len(payload) < 2 {
				return nil, fmt.Errorf("%w: truncated STAP-A length", ErrMalformed)
			}
			n := int(binary.BigEndian.Uint16(payload))
			payload = payload[2:]
			if n == 0 || n > len(payload) {
				return nil, fmt.Errorf("%w: invalid STAP-A NAL size", ErrMalformed)
			}
			nt := payload[0] & 31
			if payload[0]&0x80 != 0 || nt == 0 || nt >= 24 {
				return nil, fmt.Errorf("%w: invalid aggregated NAL type", ErrMalformed)
			}
			d.nals = append(d.nals, append([]byte(nil), payload[:n]...))
			payload = payload[n:]
			if len(d.nals) > 1024 {
				return nil, fmt.Errorf("%w: too many NAL units", ErrMalformed)
			}
		}
	case typ == 28:
		if len(payload) < 3 {
			return nil, fmt.Errorf("%w: empty FU-A", ErrMalformed)
		}
		header := payload[1]
		start, end := header&0x80 != 0, header&0x40 != 0
		if header&0x20 != 0 || header&31 == 0 || header&31 >= 24 || (start && end) {
			return nil, fmt.Errorf("%w: invalid FU-A header", ErrMalformed)
		}
		nalHeader := (payload[0] & 0xe0) | (header & 31)
		if start {
			if d.fragment != nil {
				return nil, ErrPacketLoss
			}
			d.fragmentHeader = nalHeader
			d.fragment = append([]byte{nalHeader}, payload[2:]...)
		} else {
			if d.fragment == nil || nalHeader != d.fragmentHeader {
				return nil, ErrPacketLoss
			}
			d.fragment = append(d.fragment, payload[2:]...)
		}
		if end {
			d.nals = append(d.nals, d.fragment)
			d.fragment = nil
		}
	default:
		return nil, fmt.Errorf("%w: RTP packetization type %d", ErrUnsupported, typ)
	}
	if len(d.nals) > 1024 {
		return nil, fmt.Errorf("%w: too many NAL units", ErrMalformed)
	}
	if !p.Marker {
		return nil, nil
	}
	if d.fragment != nil || len(d.nals) == 0 {
		return nil, fmt.Errorf("%w: RTP marker before complete NAL", ErrMalformed)
	}
	au = d.nals
	d.nals = nil
	d.size = 0
	return au, nil
}

// Discontinuity invalidates reference pictures while retaining SDP/in-band
// parameter sets. Call after packet loss or before a seek within the stream.
func (d *Decoder) Discontinuity() {
	d.synced = false
	d.refs = nil
	d.hasRefFrame = false
	d.pending = nil
	d.poc = pocState{}
}
