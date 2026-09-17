package h264

import (
	"errors"
	"fmt"
)

var (
	ErrMalformed   = errors.New("h264: malformed bitstream")
	ErrUnsupported = errors.New("h264: unsupported coding tool")
	ErrNeedIDR     = errors.New("h264: waiting for an IDR picture")
)

type bitReader struct {
	data []byte
	pos  int
	err  error
}

func (b *bitReader) fail(format string, args ...any) {
	if b.err == nil {
		b.err = fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
	}
}
func (b *bitReader) bits(n int) int {
	if b.err != nil {
		return 0
	}
	if n < 0 || n > 31 || n > len(b.data)*8-b.pos {
		b.fail("truncated bit field at bit %d (%d bits)", b.pos, n)
		return 0
	}
	v := 0
	for ; n > 0; n-- {
		v = v<<1 | int(b.data[b.pos/8]>>uint(7-b.pos%8)&1)
		b.pos++
	}
	return v
}
func (b *bitReader) flag() bool { return b.bits(1) != 0 }
func (b *bitReader) ue() int {
	zeros := 0
	for b.err == nil && b.bits(1) == 0 {
		zeros++
		if zeros > 30 {
			b.fail("Exp-Golomb overflow")
			return 0
		}
	}
	return (1 << uint(zeros)) - 1 + b.bits(zeros)
}
func (b *bitReader) se() int {
	v := b.ue()
	if v&1 != 0 {
		return (v + 1) / 2
	}
	return -v / 2
}
func (b *bitReader) rangeUE(max int) int {
	v := b.ue()
	if v > max {
		b.fail("unsigned value %d exceeds %d", v, max)
		return 0
	}
	return v
}
func (b *bitReader) rangeSE(min, max int) int {
	v := b.se()
	if v < min || v > max {
		b.fail("signed value %d outside [%d,%d]", v, min, max)
		return 0
	}
	return v
}
func (b *bitReader) more() bool {
	if b.err != nil || b.pos >= len(b.data)*8 {
		return false
	}
	p := b.pos
	if b.data[p/8]>>uint(7-p%8)&1 == 0 {
		return true
	}
	for p++; p < len(b.data)*8; p++ {
		if b.data[p/8]>>uint(7-p%8)&1 != 0 {
			return true
		}
	}
	return false
}
func (b *bitReader) trailing() {
	if b.bits(1) != 1 {
		b.fail("missing rbsp_stop_one_bit")
	}
	for b.pos%8 != 0 && b.err == nil {
		if b.bits(1) != 0 {
			b.fail("nonzero rbsp_alignment_zero_bit")
		}
	}
	if b.pos != len(b.data)*8 {
		b.fail("data after RBSP trailing bits")
	}
}

func rbsp(nal []byte) ([]byte, error) {
	if len(nal) < 2 || nal[0]&0x80 != 0 {
		return nil, fmt.Errorf("%w: invalid NAL header", ErrMalformed)
	}
	out := make([]byte, 0, len(nal)-1)
	zeros := 0
	for i := 1; i < len(nal); i++ {
		v := nal[i]
		if zeros == 2 {
			if v == 3 {
				// A CABAC zero word may end with an emulation-prevention byte
				// without a following byte (7.4.1 and 9.3.4.6).
				if i+1 == len(nal) && (nal[0]&31 == 1 || nal[0]&31 == 5) {
					break
				}
				if i+1 >= len(nal) || nal[i+1] > 3 {
					return nil, fmt.Errorf("%w: invalid emulation prevention byte", ErrMalformed)
				}
				zeros = 0
				continue
			}
			if v < 3 {
				return nil, fmt.Errorf("%w: unescaped start code in NAL", ErrMalformed)
			}
		}
		out = append(out, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out, nil
}
