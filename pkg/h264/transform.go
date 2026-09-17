package h264

import "fmt"

var scan4 = [16]int{0, 1, 4, 8, 5, 2, 3, 6, 9, 12, 13, 10, 7, 11, 14, 15}
var norm4 = [6][3]int{{10, 13, 16}, {11, 14, 18}, {13, 16, 20}, {14, 18, 23}, {16, 20, 25}, {18, 23, 29}}
var chromaQP = [22]int{29, 30, 31, 32, 32, 33, 34, 34, 35, 35, 36, 36, 37, 37, 37, 38, 38, 38, 39, 39, 39, 39}

func qpc(qp, offset int) int {
	q := clip(qp+offset, 0, 51)
	if q < 30 {
		return q
	}
	return chromaQP[q-30]
}
func scale4(c [16]int, qp int, dc *int, weights *[64]uint8) [16]int {
	var out [16]int
	for i, v := range c {
		p := scan4[i]
		k := 0
		if (p/4)%2 == 1 && p%2 == 1 {
			k = 2
		} else if (p/4)%2 == 1 || p%2 == 1 {
			k = 1
		}
		out[p] = rescale(int64(v)*int64(norm4[qp%6][k])*int64(weights[i]), qp/6-4, true)
	}
	if dc != nil {
		out[0] = *dc
	}
	return out
}
func inverse4(c [16]int) [16]int {
	var t, out [16]int
	for y := 0; y < 4; y++ {
		i := 4 * y
		a, b := c[i]+c[i+2], c[i]-c[i+2]
		d, e := (c[i+1]>>1)-c[i+3], c[i+1]+(c[i+3]>>1)
		t[i], t[i+1], t[i+2], t[i+3] = a+e, b+d, b-d, a-e
	}
	for x := 0; x < 4; x++ {
		a, b := t[x]+t[8+x], t[x]-t[8+x]
		d, e := (t[4+x]>>1)-t[12+x], t[4+x]+(t[12+x]>>1)
		out[x], out[4+x], out[8+x], out[12+x] = (a+e+32)>>6, (b+d+32)>>6, (b-d+32)>>6, (a-e+32)>>6
	}
	return out
}
func lumaDC(c [16]int, qp, weight int) [16]int {
	var t, f, out [16]int
	for i, v := range c {
		t[scan4[i]] = v
	}
	had := func(a, b, c, d int) (int, int, int, int) {
		return a + b + c + d, a + b - c - d, a - b - c + d, a - b + c - d
	}
	for y := 0; y < 4; y++ {
		i := y * 4
		f[i], f[i+1], f[i+2], f[i+3] = had(t[i], t[i+1], t[i+2], t[i+3])
	}
	for x := 0; x < 4; x++ {
		out[x], out[4+x], out[8+x], out[12+x] = had(f[x], f[4+x], f[8+x], f[12+x])
	}
	for i, v := range out {
		if v < -32768 || v > 32767 {
			out[i] = 32768
			continue
		}
		out[i] = rescale(int64(v)*int64(norm4[qp%6][0])*int64(weight), qp/6-6, true)
	}
	return out
}
func chromaDC(c [16]int, qp, weight int) [4]int {
	a, b, c0, d := c[0]+c[1], c[0]-c[1], c[2]+c[3], c[2]-c[3]
	out := [4]int{a + c0, b + d, a - c0, b - d}
	for i, v := range out {
		if v < -32768 || v > 32767 {
			out[i] = 32768
			continue
		}
		out[i] = rescale(int64(v)*int64(norm4[qp%6][0])*int64(weight), qp/6-5, false)
	}
	return out
}

func checkScaled(coeff []int) error {
	for _, v := range coeff {
		if v < -32768 || v > 32767 {
			return fmt.Errorf("%w: scaled transform coefficient outside 8-bit range", ErrMalformed)
		}
	}
	return nil
}
func (w *workPicture) addResidual4(addr, plane, x, y int, c [16]int, qp int, dc *int) error {
	scaled := scale4(c, qp, dc, &w.header.scaling[scalingIndex(&w.mbs[addr], plane)])
	if err := checkScaled(scaled[:]); err != nil {
		return err
	}
	w.addBlock(addr, plane, x, y, inverse4(scaled))
	return nil
}
