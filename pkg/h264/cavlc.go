package h264

import "strings"

type vlcTable map[uint32]int

func makeVLC(codes string) vlcTable {
	t := vlcTable{}
	for v, s := range strings.Fields(codes) {
		putVLC(t, s, v)
	}
	return t
}
func putVLC(t vlcTable, s string, v int) {
	if s == "" {
		return
	}
	k := uint32(1)
	for _, c := range s {
		k = k<<1 | uint32(c-'0')
	}
	t[k] = v
}
func readVLC(b *bitReader, t vlcTable) int {
	k := uint32(1)
	for n := 1; n <= 16 && b.err == nil; n++ {
		k = k<<1 | uint32(b.bits(1))
		if v, ok := t[k]; ok {
			return v
		}
	}
	b.fail("invalid CAVLC codeword at bit %d", b.pos)
	return 0
}

var coeffTokens = func() [4]vlcTable {
	var ts [4]vlcTable
	for i, rows := range coeffTokenCodes {
		ts[i] = vlcTable{}
		for total, row := range rows {
			for ones, code := range row {
				putVLC(ts[i], code, total*4+ones)
			}
		}
	}
	return ts
}()

// ITU-T H.264 (08/2021), Tables 9-7, 9-8, 9-9(a) and 9-10.
var totalZeros = []vlcTable{
	nil,
	makeVLC("1 011 010 0011 0010 00011 00010 000011 000010 0000011 0000010 00000011 00000010 000000011 000000010 000000001"),
	makeVLC("111 110 101 100 011 0101 0100 0011 0010 00011 00010 000011 000010 000001 000000"),
	makeVLC("0101 111 110 101 0100 0011 100 011 0010 00011 00010 000001 00001 000000"),
	makeVLC("00011 111 0101 0100 110 101 100 0011 011 0010 00010 00001 00000"),
	makeVLC("0101 0100 0011 111 110 101 100 011 0010 00001 0001 00000"),
	makeVLC("000001 00001 111 110 101 100 011 010 0001 001 000000"),
	makeVLC("000001 00001 101 100 011 11 010 0001 001 000000"),
	makeVLC("000001 0001 00001 011 11 10 010 001 000000"),
	makeVLC("000001 000000 0001 11 10 001 01 00001"),
	makeVLC("00001 00000 001 11 10 01 0001"),
	makeVLC("0000 0001 001 010 1 011"),
	makeVLC("0000 0001 01 1 001"),
	makeVLC("000 001 1 01"),
	makeVLC("00 01 1"),
	makeVLC("0 1"),
}
var chromaZeros = []vlcTable{nil, makeVLC("1 01 001 000"), makeVLC("1 01 00"), makeVLC("1 0")}
var runBefore = []vlcTable{nil,
	makeVLC("1 0"), makeVLC("1 01 00"), makeVLC("11 10 01 00"), makeVLC("11 10 01 001 000"),
	makeVLC("11 10 011 010 001 000"), makeVLC("11 000 001 011 010 101 100"),
	makeVLC("111 110 101 100 011 010 001 0001 00001 000001 0000001 00000001 000000001 0000000001 00000000001"),
}

// residual returns coefficients in scan order, with leading zeroes included.
func residual(b *bitReader, nc, maxCoeff int) ([16]int, int) {
	var coeff, levels, runs [16]int
	token := 0
	switch {
	case nc < 0:
		token = readVLC(b, coeffTokens[3])
	case nc < 2:
		token = readVLC(b, coeffTokens[0])
	case nc < 4:
		token = readVLC(b, coeffTokens[1])
	case nc < 8:
		token = readVLC(b, coeffTokens[2])
	default:
		v := b.bits(6)
		if v != 3 {
			token = ((v>>2)+1)*4 + (v & 3)
		}
	}
	total, ones := token/4, token%4
	if total > maxCoeff || ones > total {
		b.fail("invalid coefficient count %d/%d", total, ones)
		return coeff, 0
	}
	if total == 0 || b.err != nil {
		return coeff, 0
	}
	for i := 0; i < ones; i++ {
		levels[i] = 1 - 2*b.bits(1)
	}
	suffix := 0
	if total > 10 && ones < 3 {
		suffix = 1
	}
	for i := ones; i < total && b.err == nil; i++ {
		prefix := 0
		for b.err == nil && b.bits(1) == 0 {
			prefix++
			if prefix > 19 {
				b.fail("CAVLC level overflow")
				break
			}
		}
		n := suffix
		if prefix == 14 && suffix == 0 {
			n = 4
		} else if prefix >= 15 {
			n = prefix - 3
		}
		code := (min(15, prefix) << suffix) + b.bits(n)
		if prefix >= 15 && suffix == 0 {
			code += 15
		}
		if prefix >= 16 {
			code += (1 << (prefix - 3)) - 4096
		}
		if i == ones && ones < 3 {
			code += 2
		}
		if code&1 == 0 {
			levels[i] = (code + 2) / 2
		} else {
			levels[i] = -(code + 1) / 2
		}
		if levels[i] < -32768 || levels[i] > 32767 {
			b.fail("coefficient level out of range")
		}
		if suffix == 0 {
			suffix = 1
		}
		if abs(levels[i]) > 3<<(suffix-1) && suffix < 6 {
			suffix++
		}
	}
	zeros := 0
	if total < maxCoeff {
		if maxCoeff == 4 {
			zeros = readVLC(b, chromaZeros[total])
		} else {
			zeros = readVLC(b, totalZeros[total])
		}
	}
	if zeros+total > maxCoeff {
		b.fail("CAVLC zeros exceed block size")
		return coeff, total
	}
	for i := 0; i < total-1 && zeros > 0 && b.err == nil; i++ {
		runs[i] = readVLC(b, runBefore[min(zeros, 7)])
		zeros -= runs[i]
		if zeros < 0 {
			b.fail("CAVLC run exceeds zeros left")
		}
	}
	runs[total-1] = zeros
	pos := -1
	for i := total - 1; i >= 0 && b.err == nil; i-- {
		pos += runs[i] + 1
		if pos < 0 || pos >= maxCoeff {
			b.fail("CAVLC scan overflow")
			break
		}
		coeff[pos] = levels[i]
	}
	return coeff, total
}
func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
func clip(v, lo, hi int) int { return min(max(v, lo), hi) }
