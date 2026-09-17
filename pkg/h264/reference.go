package h264

import (
	"fmt"
	"image"
	"sort"
)

type reference struct {
	img            *image.YCbCr
	frameNum, long int
	poc            int
	id             uint64
	colocated      [][16]colocatedMotion
}

// No reference pointers: motion history must not retain an unbounded chain of
// old picture buffers. Only the first available list is used by direct modes.
type colocatedMotion struct {
	id       uint64
	x, y     int16
	refIndex int8
}
type marking struct{ op, difference, longPic, longIndex, maxLong int }
type weight struct{ value, offset, denom [3]int }

func (d *Decoder) parseReferences(b *bitReader, h *sliceHeader) error {
	h.numRefs = h.pps.refs
	if h.typ == 0 || h.typ == 1 {
		if h.typ == 1 {
			h.directSpatial = b.flag()
		}
		if b.flag() {
			h.numRefs[0] = b.rangeUE(31) + 1
			if h.typ == 1 {
				h.numRefs[1] = b.rangeUE(31) + 1
			}
		}
		nlists := 1
		if h.typ == 1 {
			nlists = 2
		}
		for list := 0; list < nlists; list++ {
			h.list[list] = d.initialList(h, list)
		}
		if nlists == 2 && len(h.list[1]) > 1 {
			same := true
			for i := range h.list[0] {
				if h.list[0][i] != h.list[1][i] {
					same = false
					break
				}
			}
			if same {
				h.list[1][0], h.list[1][1] = h.list[1][1], h.list[1][0]
			}
		}
		for list := 0; list < nlists; list++ {
			if err := d.modifyList(b, h, list); err != nil {
				return err
			}
		}
		if h.typ == 0 && h.pps.weighted || h.typ == 1 && h.pps.weightedB == 1 {
			ld, cd := b.rangeUE(7), b.rangeUE(7)
			for list := 0; list < nlists; list++ {
				h.weights[list] = make([]weight, h.numRefs[list])
				for i := range h.weights[list] {
					wt := &h.weights[list][i]
					wt.denom = [3]int{ld, cd, cd}
					wt.value = [3]int{1 << ld, 1 << cd, 1 << cd}
					if b.flag() {
						wt.value[0] = b.rangeSE(-128, 127)
						wt.offset[0] = b.rangeSE(-128, 127)
					}
					if b.flag() {
						for p := 1; p < 3; p++ {
							wt.value[p] = b.rangeSE(-128, 127)
							wt.offset[p] = b.rangeSE(-128, 127)
						}
					}
				}
			}
		}
	}
	if h.refIDC != 0 {
		if h.idr {
			h.discardPrior = b.flag()
			h.longIDR = b.flag()
		} else if b.flag() {
			h.adaptive = true
			for i := 0; b.err == nil; i++ {
				op := b.rangeUE(6)
				if op == 0 {
					break
				}
				if i >= 32 {
					b.fail("too many memory management operations")
					break
				}
				m := marking{op: op}
				if op == 1 || op == 3 {
					m.difference = b.rangeUE((1<<h.sps.logFrame)-1) + 1
				}
				if op == 2 {
					m.longPic = b.rangeUE(31)
				}
				if op == 3 || op == 6 {
					m.longIndex = b.rangeUE(15)
				}
				if op == 4 {
					m.maxLong = b.rangeUE(16)
				}
				h.marking = append(h.marking, m)
			}
		}
	}
	return b.err
}
func (d *Decoder) storeReference(w *workPicture) error {
	h := w.header
	if h.idr {
		d.refs = nil
	}
	if h.refIDC == 0 {
		return nil
	}
	d.nextReferenceID++
	current := &reference{img: w.img, frameNum: h.frameNum, long: -1, poc: h.poc, id: d.nextReferenceID}
	current.colocated = make([][16]colocatedMotion, len(w.mbs))
	for addr := range w.mbs {
		m := &w.mbs[addr]
		for i := 0; i < 16; i++ {
			c := &current.colocated[addr][i]
			c.refIndex = -1
			for list := 0; list < 2; list++ {
				if r := m.ref[list][i]; r != nil {
					*c = colocatedMotion{id: r.id, x: int16(m.mv[list][i].x), y: int16(m.mv[list][i].y), refIndex: int8(m.refIndex[list][i])}
					break
				}
			}
		}
	}
	remove := func(match func(*reference) bool) {
		dst := d.refs[:0]
		for _, r := range d.refs {
			if !match(r) {
				dst = append(dst, r)
			}
		}
		d.refs = dst
	}
	if h.idr && h.longIDR {
		current.long = 0
	}
	for _, m := range h.marking {
		short := (h.frameNum - m.difference + (1 << h.sps.logFrame)) % (1 << h.sps.logFrame)
		switch m.op {
		case 1:
			remove(func(r *reference) bool { return r.long < 0 && r.frameNum == short })
		case 2:
			remove(func(r *reference) bool { return r.long == m.longPic })
		case 3:
			remove(func(r *reference) bool { return r.long == m.longIndex })
			found := false
			for _, r := range d.refs {
				if r.long < 0 && r.frameNum == short {
					r.long = m.longIndex
					found = true
				}
			}
			if !found {
				return fmt.Errorf("%w: MMCO refers to missing short-term picture", ErrMalformed)
			}
		case 4:
			remove(func(r *reference) bool { return r.long >= m.maxLong })
		case 5:
			d.refs = nil
			current.frameNum = 0
			current.poc = 0
		case 6:
			remove(func(r *reference) bool { return r.long == m.longIndex })
			current.long = m.longIndex
		}
	}
	if !h.adaptive && len(d.refs) >= max(h.sps.refs, 1) {
		idx, oldest := -1, 1<<30
		for i, r := range d.refs {
			if r.long >= 0 {
				continue
			}
			n := r.frameNum
			if n > h.frameNum {
				n -= 1 << h.sps.logFrame
			}
			if n < oldest {
				idx, oldest = i, n
			}
		}
		if idx < 0 {
			return fmt.Errorf("%w: reference picture buffer is full", ErrMalformed)
		}
		d.refs = append(d.refs[:idx], d.refs[idx+1:]...)
	}
	if len(d.refs) >= max(h.sps.refs, 1) {
		return fmt.Errorf("%w: reference picture buffer overflow", ErrMalformed)
	}
	d.refs = append(d.refs, current)
	d.lastRefFrameNum = current.frameNum
	d.hasRefFrame = true
	return nil
}

func (d *Decoder) initialList(h *sliceHeader, list int) []*reference {
	result := append([]*reference(nil), d.refs...)
	wrap := func(r *reference) int {
		n := r.frameNum
		if n > h.frameNum {
			n -= 1 << h.sps.logFrame
		}
		return n
	}
	sort.SliceStable(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.long >= 0 {
			return b.long >= 0 && a.long < b.long
		}
		if b.long >= 0 {
			return true
		}
		if h.typ == 0 {
			return wrap(a) > wrap(b)
		}
		if list == 0 {
			if (a.poc < h.poc) != (b.poc < h.poc) {
				return a.poc < h.poc
			}
			if a.poc < h.poc {
				return a.poc > b.poc
			}
			return a.poc < b.poc
		}
		if (a.poc > h.poc) != (b.poc > h.poc) {
			return a.poc > h.poc
		}
		if a.poc > h.poc {
			return a.poc < b.poc
		}
		return a.poc > b.poc
	})
	return result
}

func (d *Decoder) modifyList(b *bitReader, h *sliceHeader, list int) error {
	if !b.flag() {
		return b.err
	}
	pred, idx := h.frameNum, 0
	for b.err == nil {
		op := b.rangeUE(3)
		if op == 3 {
			break
		}
		if idx >= h.numRefs[list] {
			b.fail("too many reference list modifications")
			break
		}
		var chosen *reference
		if op <= 1 {
			diff := b.rangeUE((1<<h.sps.logFrame)-1) + 1
			if op == 0 {
				pred = (pred - diff + (1 << h.sps.logFrame)) % (1 << h.sps.logFrame)
			} else {
				pred = (pred + diff) % (1 << h.sps.logFrame)
			}
			for _, r := range d.refs {
				if r.long < 0 && r.frameNum == pred {
					chosen = r
					break
				}
			}
		} else {
			n := b.rangeUE(31)
			for _, r := range d.refs {
				if r.long == n {
					chosen = r
					break
				}
			}
		}
		if chosen == nil {
			return fmt.Errorf("%w: missing modified reference picture", ErrMalformed)
		}
		old := h.list[list]
		result := make([]*reference, 0, len(old)+1)
		result = append(result, old[:min(idx, len(old))]...)
		result = append(result, chosen)
		for _, r := range old[min(idx, len(old)):] {
			if r != chosen {
				result = append(result, r)
			}
		}
		h.list[list] = result
		idx++
	}
	return b.err
}
