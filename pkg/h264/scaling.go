package h264

// Lists retain the bitstream's scan order. The transforms scan coefficient and
// weight entries together, avoiding a second scan on every parameter change.
type scalingLists [8][64]uint8
type scalingSyntax struct {
	present             bool
	lists               scalingLists
	specified, defaults [8]bool
}

// ITU-T H.264 (08/2021), Tables 7-3 and 7-4 (scan order).
var defaultScaling4 = [2][16]uint8{
	{6, 13, 13, 20, 20, 20, 28, 28, 28, 28, 32, 32, 32, 37, 37, 42},
	{10, 14, 14, 20, 20, 20, 24, 24, 24, 24, 27, 27, 27, 30, 30, 34},
}
var defaultScaling8 = [2][64]uint8{
	{6, 10, 10, 13, 11, 13, 16, 16, 16, 16, 18, 18, 18, 18, 18, 23, 23, 23, 23, 23, 23, 25, 25, 25, 25, 25, 25, 25, 27, 27, 27, 27, 27, 27, 27, 27, 29, 29, 29, 29, 29, 29, 29, 31, 31, 31, 31, 31, 31, 33, 33, 33, 33, 33, 36, 36, 36, 36, 38, 38, 38, 40, 40, 42},
	{9, 13, 13, 15, 13, 15, 17, 17, 17, 17, 19, 19, 19, 19, 19, 21, 21, 21, 21, 21, 21, 22, 22, 22, 22, 22, 22, 22, 24, 24, 24, 24, 24, 24, 24, 24, 25, 25, 25, 25, 25, 25, 25, 27, 27, 27, 27, 27, 27, 28, 28, 28, 28, 28, 30, 30, 30, 30, 32, 32, 32, 33, 33, 35},
}

func parseScaling(b *bitReader, count int) scalingSyntax {
	s := scalingSyntax{present: b.flag()}
	if !s.present {
		return s
	}
	for i := 0; i < count; i++ {
		s.specified[i] = b.flag()
		if !s.specified[i] {
			continue
		}
		n := 16
		if i >= 6 {
			n = 64
		}
		last, next := 8, 8
		for j := 0; j < n && b.err == nil; j++ {
			if next != 0 {
				next = (last + b.rangeSE(-128, 127) + 256) % 256
				if j == 0 && next == 0 {
					s.defaults[i] = true
				}
			}
			if next != 0 {
				last = next
			}
			s.lists[i][j] = uint8(last)
		}
	}
	return s
}

func defaultList(i int) [64]uint8 {
	if i >= 6 {
		return defaultScaling8[i-6]
	}
	var out [64]uint8
	copy(out[:], defaultScaling4[i/3][:])
	return out
}

// Table 7-2: rule A starts the Y lists at defaults; rule B inherits those
// lists from the SPS. Chroma inherits the preceding picture-level list.
func resolveScaling(seq, pic scalingSyntax) scalingLists {
	var out scalingLists
	for i := range out {
		for j := range out[i] {
			out[i][j] = 16
		}
	}
	apply := func(s scalingSyntax, inherit bool) {
		for i := range out {
			switch {
			case s.defaults[i]:
				out[i] = defaultList(i)
			case s.specified[i]:
				out[i] = s.lists[i]
			case i == 0 || i == 3 || i >= 6:
				if !inherit {
					out[i] = defaultList(i)
				}
			default:
				out[i] = out[i-1]
			}
		}
	}
	if seq.present {
		apply(seq, false)
	}
	if pic.present {
		apply(pic, seq.present)
	}
	return out
}
func scalingIndex(m *macroblock, plane int) int { return plane + 3*boolInt(!m.intra) }

func rescale(value int64, shift int, round bool) int {
	if shift >= 0 {
		value <<= shift
	} else {
		if round {
			value += 1 << (-shift - 1)
		}
		value >>= -shift
	}
	// Preserve an out-of-range sentinel for checkScaled without overflowing an
	// int on ARMv7. Valid 8-bit residual coefficients fit in signed 16 bits.
	return int(max(int64(-32769), min(int64(32768), value)))
}
