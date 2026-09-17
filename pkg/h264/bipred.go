package h264

import "fmt"

type bPartition struct {
	parts []partition
	mode  int // 0: direct; 1: list 0; 2: list 1; 3: both lists.
	refs  [2]int
}

func (w *workPicture) readBInter(e *entropyReader, addr, typ int, h *sliceHeader) error {
	m := &w.mbs[addr]
	for list := 0; list < 2; list++ {
		for i := range m.refIndex[list] {
			m.refIndex[list][i] = -1
		}
	}
	var groups []bPartition
	switch {
	case typ == 0:
		m.direct = true
		for i := 0; i < 4; i++ {
			groups = append(groups, bPartition{parts: []partition{{i % 2 * 8, i / 2 * 8, 8, 8, 0}}})
		}
	case typ <= 3:
		groups = []bPartition{{parts: []partition{{0, 0, 16, 16, 0}}, mode: typ}}
	case typ <= 21:
		modes := [9][2]int{{1, 1}, {2, 2}, {1, 2}, {2, 1}, {1, 3}, {2, 3}, {3, 1}, {3, 2}, {3, 3}}
		for i, mode := range modes[(typ-4)/2] {
			p := partition{0, i * 8, 16, 8, 0}
			if typ%2 == 1 {
				p = partition{i * 8, 0, 8, 16, 0}
			}
			groups = append(groups, bPartition{parts: []partition{p}, mode: mode})
		}
	case typ == 22:
		for i := 0; i < 4; i++ {
			sub := e.subType()
			mode, sw, sh := sub, 8, 8
			if sub >= 4 && sub <= 9 {
				mode = (sub-4)/2 + 1
				if sub%2 == 0 {
					sh = 4
				} else {
					sw = 4
				}
			}
			if sub >= 10 {
				mode = sub - 9
				sw, sh = 4, 4
			}
			if sub >= 4 {
				m.smallInter = true
			}
			var parts []partition
			for y := 0; y < 8; y += sh {
				for x := 0; x < 8; x += sw {
					parts = append(parts, partition{i%2*8 + x, i/2*8 + y, sw, sh, 0})
				}
			}
			groups = append(groups, bPartition{parts: parts, mode: mode})
		}
	default:
		return fmt.Errorf("%w: B macroblock type %d", ErrMalformed, typ)
	}
	for _, g := range groups {
		if g.mode == 0 {
			p := g.parts[0]
			if !h.sps.direct8 {
				m.smallInter = true
			}
			for y := p.y; y < p.y+8; y += 4 {
				for x := p.x; x < p.x+8; x += 4 {
					m.directBlock[blockIndex(x, y)] = true
				}
			}
			if err := w.directMotion(addr, p, h); err != nil {
				return err
			}
		}
	}
	for list := 0; list < 2; list++ {
		for i := range groups {
			g := &groups[i]
			if g.mode&(1<<list) == 0 {
				continue
			}
			ref := 0
			if h.numRefs[list] > 1 {
				ref = e.refIndex(addr, g.parts[0], list)
			}
			g.refs[list] = ref
			for _, p := range g.parts {
				for y := p.y; y < p.y+p.h; y += 4 {
					for x := p.x; x < p.x+p.w; x += 4 {
						m.refIndex[list][blockIndex(x, y)] = ref
					}
				}
			}
		}
	}
	for list := 0; list < 2; list++ {
		// Direct motion is derived from outside this macroblock. Its values may
		// be prepared early, but become available to predictors in raster order.
		clear(m.motionSet[list][:])
		for _, g := range groups {
			if g.mode&(1<<list) == 0 {
				for _, p := range g.parts {
					for y := p.y; y < p.y+p.h; y += 4 {
						for x := p.x; x < p.x+p.w; x += 4 {
							m.motionSet[list][blockIndex(x, y)] = true
						}
					}
				}
				continue
			}
			for _, p := range g.parts {
				p.ref = g.refs[list]
				pred := w.motionPredict(addr, p, list)
				delta := motion{e.mvd(addr, p, 0, list), e.mvd(addr, p, 1, list)}
				if e.b.err != nil {
					return e.b.err
				}
				mv := motion{int(int16(pred.x + delta.x)), int(int16(pred.y + delta.y))}
				for y := p.y; y < p.y+p.h; y += 4 {
					for x := p.x; x < p.x+p.w; x += 4 {
						m.mvd[list][blockIndex(x, y)] = delta
					}
				}
				if err := w.setMotion(addr, p, mv, h, list); err != nil {
					return err
				}
			}
		}
	}
	if e != nil && e.b.err != nil {
		return e.b.err
	}
	return w.compensate(addr, partition{0, 0, 16, 16, 0}, h)
}

func (w *workPicture) directMotion(addr int, group partition, h *sliceHeader) error {
	if len(h.list[0]) == 0 || len(h.list[1]) == 0 {
		return fmt.Errorf("%w: missing direct prediction references", ErrMalformed)
	}
	col := h.list[1][0]
	if col.img.Rect != w.img.Rect || addr >= len(col.colocated) {
		return fmt.Errorf("%w: colocated picture dimensions differ", ErrMalformed)
	}
	refs := [2]int{-1, -1}
	var predicted [2]motion
	zero := false
	if h.directSpatial {
		for list := 0; list < 2; list++ {
			a, b, c := w.motionAt(addr, -1, 0, list), w.motionAt(addr, 0, -1, list), w.motionAt(addr, 16, -1, list)
			if !c.available {
				c = w.motionAt(addr, -1, -1, list)
			}
			for _, n := range []neighbourMotion{a, b, c} {
				if n.ref >= 0 && (refs[list] < 0 || n.ref < refs[list]) {
					refs[list] = n.ref
				}
			}
		}
		if refs[0] < 0 && refs[1] < 0 {
			refs = [2]int{0, 0}
			zero = true
		}
		for list := 0; list < 2; list++ {
			if refs[list] >= 0 {
				predicted[list] = w.motionPredict(addr, partition{0, 0, 16, 16, refs[list]}, list)
			}
		}
	}
	step := 4
	if h.sps.direct8 {
		step = 8
	}
	for y := group.y; y < group.y+8; y += step {
		for x := group.x; x < group.x+8; x += step {
			idx := blockIndex(x, y)
			if h.sps.direct8 {
				idx = 5 * (group.y/8*2 + group.x/8)
			}
			c := col.colocated[addr][idx]
			mvcol := motion{int(c.x), int(c.y)}
			vectors := predicted
			if h.directSpatial {
				colZero := col.long < 0 && c.refIndex == 0 && abs(mvcol.x) <= 1 && abs(mvcol.y) <= 1
				for list := 0; list < 2; list++ {
					if zero || refs[list] < 0 || refs[list] == 0 && colZero {
						vectors[list] = motion{}
					}
				}
			} else {
				refs = [2]int{0, 0}
				if c.refIndex >= 0 {
					refs[0] = -1
					for i, r := range h.list[0] {
						if i >= h.numRefs[0] {
							break
						}
						if r.id == c.id {
							refs[0] = i
							break
						}
					}
					if refs[0] < 0 {
						return fmt.Errorf("%w: colocated reference is absent from list 0", ErrMalformed)
					}
				}
				r := h.list[0][refs[0]]
				vectors = [2]motion{mvcol, {}}
				if r.long < 0 && col.poc != r.poc {
					td := int(max(int64(-128), min(int64(127), int64(col.poc)-int64(r.poc))))
					tb := int(max(int64(-128), min(int64(127), int64(h.poc)-int64(r.poc))))
					tx := (16384 + abs(td/2)) / td
					factor := clip((tb*tx+32)>>6, -1024, 1023)
					vectors[0] = motion{(factor*mvcol.x + 128) >> 8, (factor*mvcol.y + 128) >> 8}
					vectors[1] = motion{vectors[0].x - mvcol.x, vectors[0].y - mvcol.y}
				}
			}
			for list := 0; list < 2; list++ {
				if refs[list] < 0 {
					continue
				}
				if err := w.setMotion(addr, partition{x, y, step, step, refs[list]}, vectors[list], h, list); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
