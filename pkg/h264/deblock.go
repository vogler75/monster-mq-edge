package h264

// ITU-T H.264 (08/2021), Tables 8-16 and 8-17.
var alphaTable = [52]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 4, 4, 5, 6, 7, 8, 9, 10, 12, 13, 15, 17, 20, 22, 25, 28, 32, 36, 40, 45, 50, 56, 63, 71, 80, 90, 101, 113, 127, 144, 162, 182, 203, 226, 255, 255}
var betaTable = [52]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13, 14, 14, 15, 15, 16, 16, 17, 17, 18, 18}
var tcTable = [3][52]int{
	{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 4, 4, 4, 5, 6, 6, 7, 8, 9, 10, 11, 13},
	{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 4, 4, 5, 5, 6, 7, 8, 8, 10, 11, 12, 13, 15, 17},
	{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 4, 4, 4, 5, 6, 6, 7, 8, 9, 10, 11, 13, 14, 16, 18, 20, 23, 25},
}

func (w *workPicture) deblock() {
	for addr := range w.mbs {
		m := &w.mbs[addr]
		if m.disableDeblock == 1 {
			continue
		}
		for dir := 0; dir < 2; dir++ {
			for plane := 0; plane < 3; plane++ {
				data, stride, x, y, n := w.plane(addr, plane)
				for edge := 0; edge < n; edge += 4 {
					if plane == 0 && m.transform8 && edge%8 != 0 {
						continue
					}
					neighbor := m
					if edge == 0 {
						if (dir == 0 && x == 0) || (dir == 1 && y == 0) {
							continue
						}
						other := addr - 1
						if dir == 1 {
							other = addr - w.header.sps.widthMB
						}
						neighbor = &w.mbs[other]
						if m.disableDeblock == 2 && neighbor.sliceID != m.sliceID {
							continue
						}
					}
					qp := (m.qp[plane] + neighbor.qp[plane] + 1) / 2
					ia, ib := clip(qp+m.alpha, 0, 51), clip(qp+m.beta, 0, 51)
					alpha, beta := alphaTable[ia], betaTable[ib]
					if alpha == 0 || beta == 0 {
						continue
					}
					segment := 4
					if plane != 0 {
						segment = 2
					}
					// Boundary strength depends on the adjacent luma blocks,
					// not on the individual samples along this edge segment.
					for off := 0; off < n; off += segment {
						lx, ly := edge, off
						if dir == 1 {
							lx, ly = off, edge
						}
						if plane != 0 {
							lx *= 2
							ly *= 2
						}
						px, py := lx-1, ly
						if dir == 1 {
							px, py = lx, ly-1
						}
						if px < 0 {
							px = 15
						}
						if py < 0 {
							py = 15
						}
						qi, pi := blockIndex(lx, ly), blockIndex(px, py)
						bs := 0
						if m.intra || neighbor.intra {
							bs = 3
							if edge == 0 {
								bs = 4
							}
						} else if hasResidual(m, qi) || hasResidual(neighbor, pi) {
							bs = 2
						} else if motionBoundary(m, qi, neighbor, pi) {
							bs = 1
						}
						if bs == 0 {
							continue
						}
						step, along, pos := 1, stride, (y+off)*stride+x+edge
						if dir == 1 {
							step, along, pos = stride, 1, (y+edge)*stride+x+off
						}
						for k := 0; k < segment; k++ {
							filterEdge(data, pos+k*along, step, bs, alpha, beta, ia, plane != 0)
						}
					}
				}
			}
		}
	}
}

func motionBoundary(a *macroblock, ai int, b *macroblock, bi int) bool {
	// Either assignment of prediction lists can describe the same reference
	// pair. Filtering is needed only if neither assignment has matching motion.
	match := func(swap int) bool {
		for list := 0; list < 2; list++ {
			other := list ^ swap
			r := a.ref[list][ai]
			if r != b.ref[other][bi] {
				return false
			}
			if r != nil {
				x, y := a.mv[list][ai], b.mv[other][bi]
				if abs(x.x-y.x) >= 4 || abs(x.y-y.y) >= 4 {
					return false
				}
			}
		}
		return true
	}
	return !match(0) && !match(1)
}
func filterEdge(data []byte, pos, step, bs, alpha, beta, ia int, chroma bool) {
	p0, p1, p2 := int(data[pos-step]), int(data[pos-2*step]), int(data[pos-3*step])
	q0, q1, q2 := int(data[pos]), int(data[pos+step]), int(data[pos+2*step])
	if abs(p0-q0) >= alpha || abs(p1-p0) >= beta || abs(q1-q0) >= beta {
		return
	}
	ap, aq := abs(p2-p0) < beta, abs(q2-q0) < beta
	if bs == 4 {
		strong := !chroma && abs(p0-q0) < (alpha>>2)+2
		if strong && ap {
			p3 := int(data[pos-4*step])
			data[pos-step] = byte((p2 + 2*p1 + 2*p0 + 2*q0 + q1 + 4) >> 3)
			data[pos-2*step] = byte((p2 + p1 + p0 + q0 + 2) >> 2)
			data[pos-3*step] = byte((2*p3 + 3*p2 + p1 + p0 + q0 + 4) >> 3)
		} else {
			data[pos-step] = byte((2*p1 + p0 + q1 + 2) >> 2)
		}
		if strong && aq {
			q3 := int(data[pos+3*step])
			data[pos] = byte((p1 + 2*p0 + 2*q0 + 2*q1 + q2 + 4) >> 3)
			data[pos+step] = byte((p0 + q0 + q1 + q2 + 2) >> 2)
			data[pos+2*step] = byte((2*q3 + 3*q2 + q1 + q0 + p0 + 4) >> 3)
		} else {
			data[pos] = byte((2*q1 + q0 + p1 + 2) >> 2)
		}
		return
	}
	tc0 := tcTable[bs-1][ia]
	tc := tc0
	if chroma {
		tc++
	} else {
		if ap {
			tc++
			data[pos-2*step] = byte(p1 + clip((p2+((p0+q0+1)>>1)-2*p1)>>1, -tc0, tc0))
		}
		if aq {
			tc++
			data[pos+step] = byte(q1 + clip((q2+((p0+q0+1)>>1)-2*q1)>>1, -tc0, tc0))
		}
	}
	delta := clip(((q0-p0)*4+p1-q1+4)>>3, -tc, tc)
	data[pos-step] = byte(clip(p0+delta, 0, 255))
	data[pos] = byte(clip(q0-delta, 0, 255))
}

func hasResidual(m *macroblock, i int) bool {
	if !m.transform8 {
		return m.nz[i] != 0
	}
	for _, n := range m.nz[i/4*4 : i/4*4+4] {
		if n != 0 {
			return true
		}
	}
	return false
}
