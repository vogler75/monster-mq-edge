package h264

// ITU-T H.264 (08/2021), equations 8-317 and 8-318.
var norm8 = [6][6]int{{20, 18, 32, 19, 25, 24}, {22, 19, 35, 21, 28, 26}, {26, 23, 42, 24, 33, 31}, {28, 25, 45, 26, 35, 33}, {32, 28, 51, 30, 40, 38}, {36, 32, 58, 34, 46, 43}}
var scan8 = func() [64]int {
	var out [64]int
	n := 0
	for sum := 0; sum <= 14; sum++ {
		for j := min(7, sum); j >= max(0, sum-7); j-- {
			x, y := sum-j, j
			if sum%2 != 0 {
				x, y = y, x
			}
			out[n] = y*8 + x
			n++
		}
	}
	return out
}()

func scale8(c [64]int, qp int, weights *[64]uint8) [64]int {
	var out [64]int
	for i, v := range c {
		p := scan8[i]
		x, y := p%8, p/8
		k := 5
		switch {
		case x%4 == 0 && y%4 == 0:
			k = 0
		case x%2 == 1 && y%2 == 1:
			k = 1
		case x%4 == 2 && y%4 == 2:
			k = 2
		case (x%4 == 0 && y%2 == 1) || (y%4 == 0 && x%2 == 1):
			k = 3
		case (x%4 == 0 && y%4 == 2) || (y%4 == 0 && x%4 == 2):
			k = 4
		}
		out[p] = rescale(int64(v)*int64(norm8[qp%6][k])*int64(weights[i]), qp/6-6, true)
	}
	return out
}
func inverse8(c [64]int) [64]int {
	pass := func(d [8]int) [8]int {
		e := [8]int{d[0] + d[4], -d[3] + d[5] - d[7] - (d[7] >> 1), d[0] - d[4], d[1] + d[7] - d[3] - (d[3] >> 1), (d[2] >> 1) - d[6], -d[1] + d[7] + d[5] + (d[5] >> 1), d[2] + (d[6] >> 1), d[3] + d[5] + d[1] + (d[1] >> 1)}
		f := [8]int{e[0] + e[6], e[1] + (e[7] >> 2), e[2] + e[4], e[3] + (e[5] >> 2), e[2] - e[4], (e[3] >> 2) - e[5], e[0] - e[6], e[7] - (e[1] >> 2)}
		return [8]int{f[0] + f[7], f[2] + f[5], f[4] + f[3], f[6] + f[1], f[6] - f[1], f[4] - f[3], f[2] - f[5], f[0] - f[7]}
	}
	var tmp, out [64]int
	for y := 0; y < 8; y++ {
		d := pass([8]int(c[y*8 : y*8+8]))
		copy(tmp[y*8:], d[:])
	}
	for x := 0; x < 8; x++ {
		var d [8]int
		for y := 0; y < 8; y++ {
			d[y] = tmp[y*8+x]
		}
		d = pass(d)
		for y, v := range d {
			out[y*8+x] = (v + 32) >> 6
		}
	}
	return out
}
func (w *workPicture) reconstruct8(addr int, coeff [4][64]int, constrained bool) error {
	m := &w.mbs[addr]
	data, stride, bx, by, _ := w.plane(addr, 0)
	for i, c := range coeff {
		x, y := i%2*8, i/2*8
		if m.intra {
			if err := w.predictSmall(addr, i*4, m.mode[i*4], 8, constrained); err != nil {
				return err
			}
		}
		scaled := scale8(c, m.qp[0], &w.header.scaling[6+boolInt(!m.intra)])
		if err := checkScaled(scaled[:]); err != nil {
			return err
		}
		r := inverse8(scaled)
		for j := 0; j < 8; j++ {
			for k := 0; k < 8; k++ {
				pos := (by+y+j)*stride + bx + x + k
				data[pos] = byte(clip(int(data[pos])+r[j*8+k], 0, 255))
			}
		}
	}
	return nil
}
