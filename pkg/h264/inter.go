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
		scale := 1
		if plane != 0 {
			scale = 2
		}
		size := 4 / scale
		for by := p.y / scale; by < (p.y+p.h)/scale; by += size {
			for bx := p.x / scale; bx < (p.x+p.w)/scale; bx += size {
				// Motion, references and weights are constant within a 4x4 luma
				// block. Resolve them once, including the matching chroma block.
				block := blockIndex(bx*scale, by*scale)
				r0, r1 := m.ref[0][block], m.ref[1][block]
				if r0 == nil && r1 == nil {
					return fmt.Errorf("%w: inter partition has no reference", ErrMalformed)
				}
				var values [2][16]int
				weights, shift, round, offset := [2]int{}, 0, 0, 0
				if r0 != nil && r1 != nil {
					switch h.pps.weightedB {
					case 1:
						a, b := h.weights[0][m.refIndex[0][block]], h.weights[1][m.refIndex[1][block]]
						weights = [2]int{a.value[plane], b.value[plane]}
						shift, round = a.denom[plane]+1, 1<<a.denom[plane]
						offset = (a.offset[plane] + b.offset[plane] + 1) >> 1
					case 2:
						weight := 32
						if r0.long < 0 && r1.long < 0 && r0.poc != r1.poc {
							factor := distanceScale(h.poc, r0.poc, r1.poc)
							if factor >= -64 && factor <= 128 {
								weight = factor
							}
						}
						weights, shift, round = [2]int{64 - weight, weight}, 6, 32
					default:
						weights, shift, round = [2]int{1, 1}, 1, 1
					}
				} else {
					list := 0
					if r0 == nil {
						list = 1
					}
					weights[list] = 1
					if len(h.weights[list]) > 0 {
						wt := h.weights[list][m.refIndex[list][block]]
						weights[list], shift, offset = wt.value[plane], wt.denom[plane], wt.offset[plane]
						if shift > 0 {
							round = 1 << (shift - 1)
						}
					}
					if weights[list] == 1<<shift && offset == 0 {
						if copyPrediction(dst, stride, x+bx, y+by, m.ref[list][block], plane, m.mv[list][block], size) {
							continue
						}
					}
				}
				for list, ref := range [2]*reference{r0, r1} {
					if ref != nil {
						predictionSamples(&values[list], ref, plane, x+bx, y+by, m.mv[list][block], size)
					}
				}
				for j := 0; j < size; j++ {
					row := dst[(y+by+j)*stride+x+bx:][:size]
					for i := range row {
						k := j*size + i
						v := ((weights[0]*values[0][k] + weights[1]*values[1][k] + round) >> shift) + offset
						row[i] = byte(clip(v, 0, 255))
					}
				}
			}
		}
	}
	return nil
}

func referencePlane(ref *reference, plane int) (src []byte, stride, width, height, shift int) {
	r := ref.img
	if plane == 0 {
		return r.Y, r.YStride, r.Rect.Dx(), r.Rect.Dy(), 2
	}
	src = r.Cb
	if plane == 2 {
		src = r.Cr
	}
	return src, r.CStride, r.Rect.Dx() / 2, r.Rect.Dy() / 2, 3
}

// Integer motion with identity weighting needs only a row copy. Edge extension
// and fractional positions use the same interpolation rules as other blocks.
func copyPrediction(dst []byte, stride, x, y int, ref *reference, plane int, mv motion, size int) bool {
	src, ss, width, height, shift := referencePlane(ref, plane)
	mask := (1 << shift) - 1
	if mv.x&mask != 0 || mv.y&mask != 0 {
		return false
	}
	sx, sy := x+(mv.x>>shift), y+(mv.y>>shift)
	if sx < 0 || sy < 0 || sx+size > width || sy+size > height {
		return false
	}
	for j := 0; j < size; j++ {
		copy(dst[(y+j)*stride+x:][:size], src[(sy+j)*ss+sx:][:size])
	}
	return true
}

func predictionSamples(out *[16]int, ref *reference, plane, x, y int, mv motion, size int) {
	src, stride, width, height, shift := referencePlane(ref, plane)
	qx, qy := (x<<shift)+mv.x, (y<<shift)+mv.y
	if plane == 0 {
		for j := 0; j < size; j++ {
			for i := 0; i < size; i++ {
				out[j*size+i] = lumaSample(src, stride, width, height, qx+(i<<2), qy+(j<<2))
			}
		}
	} else {
		for j := 0; j < size; j++ {
			for i := 0; i < size; i++ {
				out[j*size+i] = chromaSample(src, stride, width, height, qx+(i<<3), qy+(j<<3))
			}
		}
	}
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
