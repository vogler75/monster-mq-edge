package h264

import "fmt"

type motion struct{ x, y int }
type partition struct{ x, y, w, h, ref int }
type neighbourMotion struct {
	mv        motion
	ref       int
	available bool
}

func (w *workPicture) motionAt(addr, x, y, list int) neighbourMotion {
	m, i := w.neighbour(addr, x, y, 0, w.mbs[addr].sliceID)
	if m == nil || (m == &w.mbs[addr] && !m.motionSet[list][i]) {
		return neighbourMotion{ref: -1}
	}
	if m.intra || m.ref[list][i] == nil {
		return neighbourMotion{ref: -1, available: true}
	}
	return neighbourMotion{m.mv[list][i], m.refIndex[list][i], true}
}
func (w *workPicture) motionPredict(addr int, p partition, list int) motion {
	a, b, c := w.motionAt(addr, p.x-1, p.y, list), w.motionAt(addr, p.x, p.y-1, list), w.motionAt(addr, p.x+p.w, p.y-1, list)
	if !c.available {
		c = w.motionAt(addr, p.x-1, p.y-1, list)
	}
	if p.w == 16 && p.h == 8 {
		if p.y == 0 && b.ref == p.ref {
			return b.mv
		}
		if p.y != 0 && a.ref == p.ref {
			return a.mv
		}
	}
	if p.w == 8 && p.h == 16 {
		if p.x == 0 && a.ref == p.ref {
			return a.mv
		}
		if p.x != 0 && c.ref == p.ref {
			return c.mv
		}
	}
	if !b.available && !c.available && a.available {
		b, c = a, a
	}
	count := 0
	var chosen motion
	for _, v := range []neighbourMotion{a, b, c} {
		if v.ref == p.ref {
			count++
			chosen = v.mv
		}
	}
	if count == 1 {
		return chosen
	}
	median := func(a, b, c int) int { return a + b + c - min(a, min(b, c)) - max(a, max(b, c)) }
	return motion{median(a.mv.x, b.mv.x, c.mv.x), median(a.mv.y, b.mv.y, c.mv.y)}
}
func (w *workPicture) setMotion(addr int, p partition, mv motion, h *sliceHeader, list int) error {
	if p.ref < 0 || p.ref >= len(h.list[list]) || p.ref >= h.numRefs[list] {
		return fmt.Errorf("%w: reference index %d unavailable", ErrMalformed, p.ref)
	}
	r := h.list[list][p.ref]
	if r.img.Rect != w.img.Rect {
		return fmt.Errorf("%w: reference picture dimensions changed without IDR", ErrMalformed)
	}
	m := &w.mbs[addr]
	for y := p.y; y < p.y+p.h; y += 4 {
		for x := p.x; x < p.x+p.w; x += 4 {
			i := blockIndex(x, y)
			m.mv[list][i] = mv
			m.ref[list][i] = r
			m.refIndex[list][i] = p.ref
			m.motionSet[list][i] = true
		}
	}
	return nil
}
func (w *workPicture) predictSkip(addr int, h *sliceHeader) error {
	w.mbs[addr].skip = true
	if h.typ == 1 {
		return w.readBInter(nil, addr, 0, h)
	}
	p := partition{0, 0, 16, 16, 0}
	mv := motion{}
	a, b := w.motionAt(addr, -1, 0, 0), w.motionAt(addr, 0, -1, 0)
	if a.available && b.available && !(a.ref == 0 && a.mv == (motion{})) && !(b.ref == 0 && b.mv == (motion{})) {
		mv = w.motionPredict(addr, p, 0)
	}
	if err := w.setMotion(addr, p, mv, h, 0); err != nil {
		return err
	}
	return w.compensate(addr, p, h)
}
func (w *workPicture) readInter(e *entropyReader, addr, typ int, h *sliceHeader) error {
	if h.typ == 1 {
		return w.readBInter(e, addr, typ, h)
	}
	b := e.b
	var groups [][]partition
	switch typ {
	case 0:
		groups = [][]partition{{{0, 0, 16, 16, 0}}}
	case 1:
		groups = [][]partition{{{0, 0, 16, 8, 0}}, {{0, 8, 16, 8, 0}}}
	case 2:
		groups = [][]partition{{{0, 0, 8, 16, 0}}, {{8, 0, 8, 16, 0}}}
	case 3, 4:
		for i := 0; i < 4; i++ {
			sub := e.subType()
			if sub != 0 {
				w.mbs[addr].smallInter = true
			}
			x, y := (i%2)*8, (i/2)*8
			sw, sh := 8, 8
			if sub == 1 || sub == 3 {
				sh = 4
			}
			if sub == 2 || sub == 3 {
				sw = 4
			}
			var g []partition
			for yy := 0; yy < 8; yy += sh {
				for xx := 0; xx < 8; xx += sw {
					g = append(g, partition{x + xx, y + yy, sw, sh, 0})
				}
			}
			groups = append(groups, g)
		}
	}
	for _, g := range groups {
		ref := 0
		if typ != 4 && h.numRefs[0] > 1 {
			ref = e.refIndex(addr, g[0], 0)
		}
		for i := range g {
			g[i].ref = ref
			p := g[i]
			for y := p.y; y < p.y+p.h; y += 4 {
				for x := p.x; x < p.x+p.w; x += 4 {
					w.mbs[addr].refIndex[0][blockIndex(x, y)] = ref
				}
			}
		}
	}
	for _, g := range groups {
		for _, p := range g {
			pred := w.motionPredict(addr, p, 0)
			delta := motion{e.mvd(addr, p, 0, 0), e.mvd(addr, p, 1, 0)}
			mv := motion{int(int16(pred.x + delta.x)), int(int16(pred.y + delta.y))}
			for y := p.y; y < p.y+p.h; y += 4 {
				for x := p.x; x < p.x+p.w; x += 4 {
					w.mbs[addr].mvd[0][blockIndex(x, y)] = delta
				}
			}
			if b.err != nil {
				return b.err
			}
			if err := w.setMotion(addr, p, mv, h, 0); err != nil {
				return err
			}
		}
	}
	if b.err != nil {
		return b.err
	}
	return w.compensate(addr, partition{0, 0, 16, 16, 0}, h)
}
func (w *workPicture) compensate(addr int, p partition, h *sliceHeader) error {
	m := &w.mbs[addr]
	for plane := 0; plane < 3; plane++ {
		dst, stride, x, y, _ := w.plane(addr, plane)
		scale, shift := 1, 2
		if plane != 0 {
			scale, shift = 2, 3
		}
		for j := p.y / scale; j < (p.y+p.h)/scale; j++ {
			for i := p.x / scale; i < (p.x+p.w)/scale; i++ {
				block := blockIndex(i*scale, j*scale)
				var values [2]int
				for list := 0; list < 2; list++ {
					ref := m.ref[list][block]
					if ref == nil {
						continue
					}
					r := ref.img
					src, ss, ww, hh := r.Y, r.YStride, r.Rect.Dx(), r.Rect.Dy()
					if plane != 0 {
						src, ss, ww, hh = r.Cb, r.CStride, ww/2, hh/2
						if plane == 2 {
							src = r.Cr
						}
					}
					mv := m.mv[list][block]
					xx, yy := ((x+i)<<shift)+mv.x, ((y+j)<<shift)+mv.y
					if plane == 0 {
						values[list] = lumaSample(src, ss, ww, hh, xx, yy)
					} else {
						values[list] = chromaSample(src, ss, ww, hh, xx, yy)
					}
				}
				r0, r1 := m.ref[0][block], m.ref[1][block]
				v := 0
				if r0 != nil && r1 != nil {
					switch h.pps.weightedB {
					case 1:
						a, b := h.weights[0][m.refIndex[0][block]], h.weights[1][m.refIndex[1][block]]
						den := a.denom[plane]
						v = ((a.value[plane]*values[0] + b.value[plane]*values[1] + (1 << den)) >> (den + 1)) + ((a.offset[plane] + b.offset[plane] + 1) >> 1)
					case 2:
						weight := 32
						if r0.long < 0 && r1.long < 0 && r0.poc != r1.poc {
							factor := distanceScale(h.poc, r0.poc, r1.poc)
							if factor >= -64 && factor <= 128 {
								weight = factor
							}
						}
						v = ((64-weight)*values[0] + weight*values[1] + 32) >> 6
					default:
						v = (values[0] + values[1] + 1) >> 1
					}
				} else {
					list := 0
					if r0 == nil {
						list = 1
					}
					if m.ref[list][block] == nil {
						return fmt.Errorf("%w: inter partition has no reference", ErrMalformed)
					}
					v = values[list]
					if len(h.weights[list]) > 0 {
						wt := h.weights[list][m.refIndex[list][block]]
						den := wt.denom[plane]
						round := 0
						if den > 0 {
							round = 1 << (den - 1)
						}
						v = ((wt.value[plane]*v + round) >> den) + wt.offset[plane]
					}
				}
				dst[(y+j)*stride+x+i] = byte(clip(v, 0, 255))
			}
		}
	}
	return nil
}

// The implicit biprediction weight is one quarter of the temporal distance
// scale (8-198, 8-279). POC subtraction is widened for 32-bit targets.
func distanceScale(current, first, second int) int {
	td := int(max(int64(-128), min(int64(127), int64(second)-int64(first))))
	tb := int(max(int64(-128), min(int64(127), int64(current)-int64(first))))
	if td == 0 {
		return 256
	}
	tx := (16384 + abs(td/2)) / td
	return clip((tb*tx+32)>>6, -1024, 1023) >> 2
}
func sample(p []byte, stride, w, h, x, y int) int {
	return int(p[clip(y, 0, h-1)*stride+clip(x, 0, w-1)])
}

var sixTap = [6]int{1, -5, 20, 20, -5, 1}

func halfSample(p []byte, stride, w, h, x, y int, horiz, vert bool) int {
	if !horiz && !vert {
		return sample(p, stride, w, h, x, y)
	}
	v := 0
	if horiz && vert {
		for j, t := range sixTap {
			row := 0
			for i, s := range sixTap {
				row += s * sample(p, stride, w, h, x+i-2, y+j-2)
			}
			v += t * row
		}
		return clip((v+512)>>10, 0, 255)
	}
	for i, t := range sixTap {
		xx, yy := x, y
		if horiz {
			xx += i - 2
		} else {
			yy += i - 2
		}
		v += t * sample(p, stride, w, h, xx, yy)
	}
	return clip((v+16)>>5, 0, 255)
}
func lumaSample(p []byte, stride, w, h, qx, qy int) int {
	x, y, fx, fy := qx>>2, qy>>2, qx&3, qy&3
	if fx%2 == 0 && fy%2 == 0 {
		return halfSample(p, stride, w, h, x, y, fx == 2, fy == 2)
	}
	a, b := 0, 0
	switch {
	case fy == 0:
		a = halfSample(p, stride, w, h, x, y, true, false)
		b = sample(p, stride, w, h, x+fx/2, y)
	case fx == 0:
		a = halfSample(p, stride, w, h, x, y, false, true)
		b = sample(p, stride, w, h, x, y+fy/2)
	case fx == 2:
		a = halfSample(p, stride, w, h, x, y, true, true)
		b = halfSample(p, stride, w, h, x, y+fy/2, true, false)
	case fy == 2:
		a = halfSample(p, stride, w, h, x, y, true, true)
		b = halfSample(p, stride, w, h, x+fx/2, y, false, true)
	default:
		a = halfSample(p, stride, w, h, x, y+fy/2, true, false)
		b = halfSample(p, stride, w, h, x+fx/2, y, false, true)
	}
	return (a + b + 1) >> 1
}
func chromaSample(p []byte, stride, w, h, qx, qy int) int {
	x, y, fx, fy := qx>>3, qy>>3, qx&7, qy&7
	a, b, c, d := sample(p, stride, w, h, x, y), sample(p, stride, w, h, x+1, y), sample(p, stride, w, h, x, y+1), sample(p, stride, w, h, x+1, y+1)
	return ((8-fx)*(8-fy)*a + fx*(8-fy)*b + (8-fx)*fy*c + fx*fy*d + 32) >> 6
}
