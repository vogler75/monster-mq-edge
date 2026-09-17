package h264

type cabacContext struct{ state, mps int }
type cabacReader struct {
	b            *bitReader
	contexts     [460]cabacContext
	span, offset int
}

// ITU-T H.264 (08/2021), Table 9-45. MPS transitions increment up to 62.
var cabacLPS = [64]int{0, 0, 1, 2, 2, 4, 4, 5, 6, 7, 8, 9, 9, 11, 11, 12, 13, 13, 15, 15, 16, 16, 18, 18, 19, 19, 21, 21, 22, 22, 23, 24, 24, 25, 26, 26, 27, 27, 28, 29, 29, 30, 30, 30, 31, 32, 32, 33, 33, 33, 34, 34, 35, 35, 35, 36, 36, 36, 37, 37, 37, 38, 38, 63}

func newCABAC(b *bitReader, h *sliceHeader) *cabacReader {
	c := &cabacReader{b: b}
	group := 0
	if h.typ != 2 {
		group = h.cabacInit + 1
	}
	for i, mn := range cabacInit[group] {
		pre := clip(((int(mn[0])*h.qp)>>4)+int(mn[1]), 1, 126)
		if pre <= 63 {
			c.contexts[i] = cabacContext{63 - pre, 0}
		} else {
			c.contexts[i] = cabacContext{pre - 64, 1}
		}
	}
	for b.pos%8 != 0 && b.err == nil {
		if b.bits(1) != 1 {
			b.fail("nonzero CABAC alignment mismatch")
		}
	}
	c.restart()
	return c
}
func (c *cabacReader) restart() {
	c.span = 510
	c.offset = c.b.bits(9)
	if c.offset >= 510 {
		c.b.fail("invalid CABAC initial offset")
	}
}
func (c *cabacReader) renorm() {
	for c.span < 256 && c.b.err == nil {
		c.span <<= 1
		c.offset = c.offset<<1 | c.b.bits(1)
	}
	if c.b.err == nil && c.offset >= c.span {
		c.b.fail("CABAC offset outside range")
	}
}
func (c *cabacReader) bin(idx int) int {
	if c.b.err != nil {
		return 0
	}
	ctx := &c.contexts[idx]
	lps := cabacRange[ctx.state][(c.span>>6)&3]
	c.span -= lps
	value := ctx.mps
	if c.offset >= c.span {
		value = 1 - value
		c.offset -= c.span
		c.span = lps
		if ctx.state == 0 {
			ctx.mps ^= 1
		}
		ctx.state = cabacLPS[ctx.state]
	} else {
		ctx.state = min(ctx.state+1, 62)
	}
	c.renorm()
	return value
}
func (c *cabacReader) bypass() int {
	if c.b.err != nil {
		return 0
	}
	c.offset = c.offset<<1 | c.b.bits(1)
	if c.offset >= c.span {
		c.offset -= c.span
		return 1
	}
	return 0
}
func (c *cabacReader) terminate() bool {
	if c.b.err != nil {
		return false
	}
	c.span -= 2
	if c.offset >= c.span {
		return true
	}
	c.renorm()
	return false
}

func (c *cabacReader) finish() {
	b := c.b
	if b.err != nil {
		return
	}
	// The arithmetic decoder's nine-bit lookahead consumes the stop bit at
	// termination (9.3.3.2.4; encoder flushing is illustrated in Figure 9-12).
	if b.pos == 0 || b.data[(b.pos-1)/8]>>uint(7-(b.pos-1)%8)&1 == 0 {
		b.fail("missing CABAC rbsp_stop_one_bit")
		return
	}
	// FFmpeg/libx264 fixtures contain nonzero unused bits in this byte.
	// Accept terminal-byte stuffing; require any subsequent CABAC words to be zero.
	b.pos = (b.pos + 7) &^ 7
	if (len(b.data)*8-b.pos)%16 != 0 {
		b.fail("incomplete cabac_zero_word")
	}
	for b.pos < len(b.data)*8 && b.err == nil {
		if b.bits(16) != 0 {
			b.fail("nonzero cabac_zero_word")
		}
	}
}
func (c *cabacReader) eg(k int) int {
	value := 0
	for c.b.err == nil && c.bypass() != 0 {
		value += 1 << k
		k++
		if k > 16 {
			c.b.fail("CABAC suffix overflow")
			return 0
		}
	}
	for k--; k >= 0; k-- {
		value += c.bypass() << k
	}
	return value
}

type entropyReader struct {
	b         *bitReader
	c         *cabacReader
	w         *workPicture
	h         *sliceHeader
	prevDelta int
}

func (e *entropyReader) neighbours(addr int) (*macroblock, *macroblock) {
	a, _ := e.w.neighbour(addr, -1, 0, 0, e.w.mbs[addr].sliceID)
	b, _ := e.w.neighbour(addr, 0, -1, 0, e.w.mbs[addr].sliceID)
	return a, b
}
func (e *entropyReader) mbType(addr int) int {
	if e.c == nil {
		if e.h.typ == 1 {
			return e.b.rangeUE(48)
		}
		return e.b.rangeUE(30)
	}
	if e.h.typ == 1 {
		a, b := e.neighbours(addr)
		ctx := 27
		if a != nil && !a.direct {
			ctx++
		}
		if b != nil && !b.direct {
			ctx++
		}
		if e.c.bin(ctx) == 0 {
			return 0
		}
		if e.c.bin(30) == 0 {
			return 1 + e.c.bin(32)
		}
		bits := e.c.bin(31)<<3 | e.c.bin(32)<<2 | e.c.bin(32)<<1 | e.c.bin(32)
		if bits < 8 {
			return 3 + bits
		}
		if bits == 13 {
			return 23 + e.intraType(addr, 32)
		}
		if bits == 14 {
			return 11
		}
		if bits == 15 {
			return 22
		}
		return 12 + (bits-8)*2 + e.c.bin(32)
	}
	if e.h.typ == 0 {
		if e.c.bin(14) == 0 {
			b := e.c.bin(15)
			if b == 0 {
				if e.c.bin(16) == 0 {
					return 0
				}
				return 3
			}
			if e.c.bin(17) == 0 {
				return 2
			}
			return 1
		}
		return 5 + e.intraType(addr, 17)
	}
	return e.intraType(addr, 3)
}
func (e *entropyReader) intraType(addr, base int) int {
	c := e.c
	ctx := base
	if base == 3 {
		a, b := e.neighbours(addr)
		if a != nil && (a.i16 || a.pcm) {
			ctx++
		}
		if b != nil && (b.i16 || b.pcm) {
			ctx++
		}
	}
	if c.bin(ctx) == 0 {
		return 0
	}
	if c.terminate() {
		return 25
	}
	lctx, cctx, mctx := base+1, base+2, base+3
	if base == 3 {
		lctx, cctx, mctx = 6, 7, 9
	}
	luma := c.bin(lctx)
	chroma := 0
	if c.bin(cctx) != 0 {
		chroma = 1 + c.bin(cctx+boolInt(base == 3))
	}
	mode := 2*c.bin(mctx) + c.bin(mctx+boolInt(base == 3))
	return 1 + mode + 4*chroma + 12*luma
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func (e *entropyReader) mode(pred int) int {
	if e.c == nil {
		if e.b.flag() {
			return pred
		}
		mode := e.b.bits(3)
		if mode >= pred {
			mode++
		}
		return mode
	}
	if e.c.bin(68) != 0 {
		return pred
	}
	mode := e.c.bin(69) | e.c.bin(69)<<1 | e.c.bin(69)<<2
	if mode >= pred {
		mode++
	}
	return mode
}
func (e *entropyReader) chromaMode(addr int) int {
	if e.c == nil {
		return e.b.rangeUE(3)
	}
	a, b := e.neighbours(addr)
	ctx := 64
	if a != nil && a.intra && !a.pcm && a.chromaMode != 0 {
		ctx++
	}
	if b != nil && b.intra && !b.pcm && b.chromaMode != 0 {
		ctx++
	}
	mode := 0
	if e.c.bin(ctx) != 0 {
		mode = 1
		for mode < 3 && e.c.bin(67) != 0 {
			mode++
		}
	}
	return mode
}
func (e *entropyReader) cbp(addr int, intra bool) int {
	if e.c == nil {
		code := e.b.rangeUE(47)
		if intra {
			return intraCBP[code]
		}
		return interCBP[code]
	}
	m := &e.w.mbs[addr]
	for i := 0; i < 4; i++ {
		x, y := i%2*8, i/2*8
		a, ai := e.w.neighbour(addr, x-1, y, 0, m.sliceID)
		b, bi := e.w.neighbour(addr, x, y-1, 0, m.sliceID)
		cond := func(n *macroblock, idx int) int {
			if n == nil || n.pcm || n.cbp&(1<<(idx/4)) != 0 {
				return 0
			}
			return 1
		}
		m.cbp |= e.c.bin(73+cond(a, ai)+2*cond(b, bi)) << i
	}
	a, b := e.neighbours(addr)
	cond := func(n *macroblock, second bool) int {
		if n == nil {
			return 0
		}
		if n.pcm {
			return 1
		}
		if second {
			return boolInt(n.cbp/16 == 2)
		}
		return boolInt(n.cbp/16 != 0)
	}
	chroma := 0
	if e.c.bin(77+cond(a, false)+2*cond(b, false)) != 0 {
		chroma = 1 + e.c.bin(81+cond(a, true)+2*cond(b, true))
	}
	m.cbp += 16 * chroma
	return m.cbp
}
func (e *entropyReader) delta() int {
	if e.c == nil {
		return e.b.rangeSE(-26, 25)
	}
	v := 0
	if e.c.bin(60+boolInt(e.prevDelta != 0)) != 0 {
		v = 1
		for e.b.err == nil && e.c.bin(62+boolInt(v > 1)) != 0 {
			v++
			if v > 52 {
				e.b.fail("CABAC QP delta overflow")
				break
			}
		}
	}
	delta := (v + 1) / 2
	if v%2 == 0 {
		delta = -delta
	}
	if delta < -26 || delta > 25 {
		e.b.fail("invalid QP delta")
	}
	e.prevDelta = delta
	return delta
}
func (e *entropyReader) coeff(addr, idx, cat, maxc int) ([16]int, int) {
	if e.c == nil {
		nc := -1
		if cat != 3 {
			nc = e.w.nc(addr, idx, e.w.mbs[addr].sliceID)
		}
		return residual(e.b, nc, maxc)
	}
	var out [16]int
	m := &e.w.mbs[addr]
	plane, x, y := 0, 0, 0
	if idx < 16 {
		x, y = blockXY(idx)
	} else {
		plane = (idx-16)/4 + 1
		x = idx % 2 * 4
		y = idx % 4 / 2 * 4
	}
	var a, b *macroblock
	ai, bi := 0, 0
	if cat == 0 || cat == 3 {
		a, b = e.neighbours(addr)
	} else {
		a, ai = e.w.neighbour(addr, x-1, y, plane, m.sliceID)
		b, bi = e.w.neighbour(addr, x, y-1, plane, m.sliceID)
	}
	cond := func(n *macroblock, i int) int {
		if n == nil {
			return boolInt(m.intra)
		}
		if n.pcm {
			return 1
		}
		if cat == 0 {
			return boolInt(n.i16 && n.dcNZ[0])
		}
		if cat == 3 {
			return boolInt(n.dcNZ[plane])
		}
		return boolInt(n.nz[i] != 0)
	}
	if e.c.bin(85+4*cat+cond(a, ai)+2*cond(b, bi)) == 0 {
		return out, 0
	}
	result, count := e.cabacCoefficients(cat, maxc)
	copy(out[:], result[:16])
	return out, count
}
func (e *entropyReader) cabacCoefficients(cat, maxc int) ([64]int, int) {
	var out [64]int
	sigBase := [6]int{105, 120, 134, 149, 152, 402}
	lastBase := [6]int{166, 181, 195, 210, 213, 417}
	levelBase := [6]int{227, 237, 247, 257, 266, 426}
	var positions [64]int
	count := 0
	for i := 0; i < maxc; i++ {
		if i == maxc-1 {
			positions[count] = i
			count++
			break
		}
		sig, last := i, i
		if cat == 5 {
			sig, last = significant8[i], last8[i]
		}
		if e.c.bin(sigBase[cat]+sig) != 0 {
			positions[count] = i
			count++
			if e.c.bin(lastBase[cat]+last) != 0 {
				break
			}
		}
	}
	ones, larger := 0, 0
	for i := count - 1; i >= 0 && e.b.err == nil; i-- {
		ctx := 0
		if larger == 0 {
			ctx = min(4, 1+ones)
		}
		level := 1
		if e.c.bin(levelBase[cat]+ctx) != 0 {
			level = 2
			ctx = levelBase[cat] + 5 + min(4-boolInt(cat == 3), larger)
			for level < 15 && e.c.bin(ctx) != 0 {
				level++
			}
			if level == 15 {
				level += e.c.eg(0)
			}
		}
		if level > 32768 {
			e.b.fail("CABAC coefficient overflow")
		}
		if level == 1 {
			ones++
		} else {
			larger++
		}
		if e.c.bypass() != 0 {
			level = -level
		}
		if level < -32768 || level > 32767 {
			e.b.fail("coefficient level out of range")
		}
		out[positions[i]] = level
	}
	return out, count
}

func (e *entropyReader) skip(addr int) bool {
	a, b := e.neighbours(addr)
	ctx := 11
	if e.h.typ == 1 {
		ctx = 24
	}
	if a != nil && !a.skip {
		ctx++
	}
	if b != nil && !b.skip {
		ctx++
	}
	return e.c.bin(ctx) != 0
}
func (e *entropyReader) subType() int {
	if e.h.typ == 1 {
		if e.c == nil {
			return e.b.rangeUE(12)
		}
		if e.c.bin(36) == 0 {
			return 0
		}
		if e.c.bin(37) == 0 {
			return 1 + e.c.bin(39)
		}
		if e.c.bin(38) == 0 {
			return 3 + 2*e.c.bin(39) + e.c.bin(39)
		}
		if e.c.bin(39) == 0 {
			return 7 + 2*e.c.bin(39) + e.c.bin(39)
		}
		return 11 + e.c.bin(39)
	}
	if e.c == nil {
		return e.b.rangeUE(3)
	}
	if e.c.bin(21) != 0 {
		return 0
	}
	if e.c.bin(22) == 0 {
		return 1
	}
	if e.c.bin(23) != 0 {
		return 2
	}
	return 3
}
func (e *entropyReader) refIndex(addr int, p partition, list int) int {
	if e.c == nil {
		if e.h.numRefs[list] == 2 {
			return 1 - e.b.bits(1)
		}
		return e.b.rangeUE(e.h.numRefs[list] - 1)
	}
	m := &e.w.mbs[addr]
	a, ai := e.w.neighbour(addr, p.x-1, p.y, 0, m.sliceID)
	b, bi := e.w.neighbour(addr, p.x, p.y-1, 0, m.sliceID)
	cond := func(n *macroblock, i int) int {
		if n == nil || n.intra || n.skip || n.directBlock[i] {
			return 0
		}
		return boolInt(n.refIndex[list][i] > 0)
	}
	ref := 0
	if e.c.bin(54+cond(a, ai)+2*cond(b, bi)) != 0 {
		ref = 1
		for e.b.err == nil && e.c.bin(58+boolInt(ref > 1)) != 0 {
			ref++
			if ref >= e.h.numRefs[list] {
				e.b.fail("CABAC reference index overflow")
				break
			}
		}
	}
	if ref >= e.h.numRefs[list] {
		e.b.fail("CABAC reference index exceeds active list")
	}
	return ref
}
func (e *entropyReader) mvd(addr int, p partition, axis, list int) int {
	if e.c == nil {
		return e.b.rangeSE(-32768, 32767)
	}
	m := &e.w.mbs[addr]
	a, ai := e.w.neighbour(addr, p.x-1, p.y, 0, m.sliceID)
	b, bi := e.w.neighbour(addr, p.x, p.y-1, 0, m.sliceID)
	component := func(n *macroblock, i int) int {
		if n == nil || n.intra || n.skip {
			return 0
		}
		if axis == 0 {
			return abs(n.mvd[list][i].x)
		}
		return abs(n.mvd[list][i].y)
	}
	sum := component(a, ai) + component(b, bi)
	ctx := 0
	if sum > 32 {
		ctx = 2
	} else if sum > 2 {
		ctx = 1
	}
	base := 40 + 7*axis
	if e.c.bin(base+ctx) == 0 {
		return 0
	}
	value := 1
	for value < 9 && e.c.bin(base+min(value+2, 6)) != 0 {
		value++
	}
	if value == 9 {
		value += e.c.eg(3)
	}
	if e.c.bypass() != 0 {
		value = -value
	}
	if value < -32768 || value > 32767 {
		e.b.fail("CABAC motion vector difference overflow")
	}
	return value
}

func (e *entropyReader) transform8(addr int) bool {
	if e.c == nil {
		return e.b.flag()
	}
	a, b := e.neighbours(addr)
	ctx := 399
	if a != nil && a.transform8 {
		ctx++
	}
	if b != nil && b.transform8 {
		ctx++
	}
	return e.c.bin(ctx) != 0
}
func (e *entropyReader) coeff8(addr, group int) [64]int {
	m := &e.w.mbs[addr]
	var out [64]int
	if e.c != nil {
		c, n := e.cabacCoefficients(5, 64)
		for i := 0; i < 4; i++ {
			m.nz[group*4+i] = n
		}
		return c
	}
	for i := 0; i < 4; i++ {
		idx := group*4 + i
		c, n := residual(e.b, e.w.nc(addr, idx, m.sliceID), 16)
		m.nz[idx] = n
		for j, v := range c {
			out[4*j+i] = v
		}
	}
	return out
}
