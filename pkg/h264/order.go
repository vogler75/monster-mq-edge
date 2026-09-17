package h264

import (
	"fmt"
	"image"
	"sort"
)

type pocState struct {
	msb, lsb, frameOffset int64
	frameNum              int
}

// derivePOC implements clause 8.2.1 for progressive frames. Calculations use
// int64 before the normative signed-32-bit bounds check, including on ARMv7.
func (d *Decoder) derivePOC(h *sliceHeader) error {
	s, prev := h.sps, d.poc
	if h.idr {
		prev = pocState{}
	}
	h.frameOffset = prev.frameOffset
	if !h.idr && prev.frameNum > h.frameNum {
		h.frameOffset += 1 << s.logFrame
	}
	var top, bottom int64
	switch s.pocType {
	case 0:
		lsb, wrap := int64(h.pocLSB), int64(1)<<s.logPOC
		h.pocMSB = prev.msb
		if lsb < prev.lsb && prev.lsb-lsb >= wrap/2 {
			h.pocMSB += wrap
		} else if lsb > prev.lsb && lsb-prev.lsb > wrap/2 {
			h.pocMSB -= wrap
		}
		top = h.pocMSB + lsb
		bottom = top + int64(h.deltaPOC[1])
	case 1:
		absolute := int64(0)
		if len(s.offsets) != 0 {
			absolute = h.frameOffset + int64(h.frameNum)
		}
		if h.refIDC == 0 && absolute > 0 {
			absolute--
		}
		if absolute > 0 {
			n := int64(len(s.offsets))
			var cycle int64
			for _, offset := range s.offsets {
				cycle += int64(offset)
			}
			top = (absolute - 1) / n * cycle
			for i := int64(0); i <= (absolute-1)%n; i++ {
				top += int64(s.offsets[i])
			}
		}
		if h.refIDC == 0 {
			top += int64(s.offsetNonRef)
		}
		top += int64(h.deltaPOC[0])
		bottom = top + int64(s.offsetTopBottom) + int64(h.deltaPOC[1])
	case 2:
		if !h.idr {
			top = 2 * (h.frameOffset + int64(h.frameNum))
			if h.refIDC == 0 {
				top--
			}
		}
		bottom = top
	}
	for _, v := range []int64{top, bottom, h.pocMSB, h.frameOffset} {
		if v < -1<<31 || v > 1<<31-1 {
			return fmt.Errorf("%w: picture order count overflow", ErrMalformed)
		}
	}
	h.topPOC, h.bottomPOC, h.poc = int(top), int(bottom), int(min(top, bottom))
	if h.idr && h.poc != 0 {
		return fmt.Errorf("%w: nonzero IDR picture order count", ErrMalformed)
	}
	return nil
}

func (h *sliceHeader) mmco5() bool {
	for _, m := range h.marking {
		if m.op == 5 {
			return true
		}
	}
	return false
}

func (d *Decoder) commitPOC(h *sliceHeader) {
	d.poc.frameNum, d.poc.frameOffset = h.frameNum, h.frameOffset
	if h.refIDC != 0 {
		d.poc.msb, d.poc.lsb = h.pocMSB, int64(h.pocLSB)
	}
	if h.mmco5() {
		d.poc = pocState{lsb: int64(h.topPOC) - int64(h.poc)}
		h.topPOC -= h.poc
		h.bottomPOC -= h.poc
		h.poc = 0
	}
}

type pendingPicture struct {
	frame *Frame
	img   *image.YCbCr
}

func (d *Decoder) drain(keep int) []*Frame {
	if len(d.pending) <= keep {
		return nil
	}
	sort.SliceStable(d.pending, func(i, j int) bool {
		return d.pending[i].frame.PictureOrderCount < d.pending[j].frame.PictureOrderCount
	})
	n := len(d.pending) - keep
	frames := make([]*Frame, n)
	for i := range frames {
		frames[i] = d.pending[i].frame
	}
	copy(d.pending, d.pending[n:])
	clear(d.pending[keep:])
	d.pending = d.pending[:keep]
	return frames
}

func (d *Decoder) bufferedPictures() int {
	n := len(d.refs)
	for _, p := range d.pending {
		found := false
		for _, r := range d.refs {
			if p.img == r.img {
				found = true
				break
			}
		}
		if !found {
			n++
		}
	}
	return n
}

// Flush returns all delayed pictures in display order at end of stream. It
// preserves references and parameter sets. Do not flush between access units:
// pictures decoded later can precede pictures currently awaiting display.
func (d *Decoder) Flush() []*Frame { return d.drain(0) }
