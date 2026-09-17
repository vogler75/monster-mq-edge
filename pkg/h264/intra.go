package h264

import "fmt"

func (w *workPicture) reconstructIntra(addr, mode16, cmode int, coeff [24][16]int, dc [16]int, constrained bool) error {
	m := &w.mbs[addr]
	if m.i16 {
		if err := w.predictLarge(addr, 0, mode16, constrained); err != nil {
			return err
		}
		dc = lumaDC(dc, m.qp[0], int(w.header.scaling[0][0]))
	}
	for i := 0; i < 16 && !m.transform8; i++ {
		x, y := blockXY(i)
		if !m.i16 {
			if err := w.predictSmall(addr, i, m.mode[i], 4, constrained); err != nil {
				return err
			}
		}
		var d *int
		if m.i16 {
			d = &dc[y/4*4+x/4]
		}
		if err := w.addResidual4(addr, 0, x, y, coeff[i], m.qp[0], d); err != nil {
			return err
		}
	}
	for p := 1; p <= 2; p++ {
		if err := w.predictLarge(addr, p, cmode, constrained); err != nil {
			return err
		}
		for i := 0; i < 4; i++ {
			c := coeff[16+(p-1)*4+i]
			if err := w.addResidual4(addr, p, (i%2)*4, (i/2)*4, c, m.qp[p], &c[0]); err != nil {
				return err
			}
		}
	}
	return nil
}
func (w *workPicture) addBlock(addr, p, x, y int, r [16]int) {
	data, stride, bx, by, _ := w.plane(addr, p)
	for j := 0; j < 4; j++ {
		for i := 0; i < 4; i++ {
			pos := (by+y+j)*stride + bx + x + i
			data[pos] = byte(clip(int(data[pos])+r[j*4+i], 0, 255))
		}
	}
}
func (w *workPicture) intraAvailable(addr, x, y, p, index int, constrained bool) bool {
	m, i := w.neighbour(addr, x, y, p, w.mbs[addr].sliceID)
	if m == nil || (constrained && !m.intra) {
		return false
	}
	if m == &w.mbs[addr] && i >= index {
		return false
	}
	return true
}
func (w *workPicture) predictSmall(addr, index, mode, n int, constrained bool) error {
	data, stride, bx, by, _ := w.plane(addr, 0)
	x, y := blockXY(index)
	topOK := w.intraAvailable(addr, x, y-1, 0, index, constrained)
	leftOK := w.intraAvailable(addr, x-1, y, 0, index, constrained)
	cornerOK := w.intraAvailable(addr, x-1, y-1, 0, index, constrained)
	rightOK := w.intraAvailable(addr, x+2*n-1, y-1, 0, index, constrained)
	var top [17]int
	var left [9]int
	if cornerOK {
		top[0] = int(data[(by+y-1)*stride+bx+x-1])
		left[0] = top[0]
	}
	if topOK {
		for i := 0; i < n; i++ {
			top[i+1] = int(data[(by+y-1)*stride+bx+x+i])
		}
		for i := n; i < 2*n; i++ {
			top[i+1] = top[n]
			if rightOK {
				top[i+1] = int(data[(by+y-1)*stride+bx+x+i])
			}
		}
	}
	if leftOK {
		for j := 0; j < n; j++ {
			left[j+1] = int(data[(by+y+j)*stride+bx+x-1])
		}
	}
	if n == 8 {
		oldTop, oldLeft := top, left
		if topOK {
			for i := 1; i <= 2*n; i++ {
				before := oldTop[max(1, i-1)]
				if i == 1 && cornerOK {
					before = oldTop[0]
				}
				top[i] = (before + 2*oldTop[i] + oldTop[min(2*n, i+1)] + 2) >> 2
			}
		}
		if leftOK {
			for i := 1; i <= n; i++ {
				before := oldLeft[max(1, i-1)]
				if i == 1 && cornerOK {
					before = oldLeft[0]
				}
				left[i] = (before + 2*oldLeft[i] + oldLeft[min(n, i+1)] + 2) >> 2
			}
		}
		if cornerOK {
			v := oldTop[0]
			if topOK && leftOK {
				v = (oldTop[1] + 2*v + oldLeft[1] + 2) >> 2
			} else if topOK {
				v = (3*v + oldTop[1] + 2) >> 2
			} else if leftOK {
				v = (3*v + oldLeft[1] + 2) >> 2
			}
			top[0], left[0] = v, v
		}
	}
	needTop := mode == 0 || mode == 3 || mode == 4 || mode == 5 || mode == 6 || mode == 7
	needLeft := mode == 1 || mode == 4 || mode == 5 || mode == 6 || mode == 8
	needCorner := mode == 4 || mode == 5 || mode == 6
	if (needTop && !topOK) || (needLeft && !leftOK) || (needCorner && !cornerOK) {
		return fmt.Errorf("%w: unavailable intra4 references at macroblock %d block %d mode %d", ErrMalformed, addr, index, mode)
	}
	dc := 128
	sum, count := 0, 0
	if topOK {
		for _, v := range top[1 : n+1] {
			sum += v
		}
		count += n
	}
	if leftOK {
		for _, v := range left[1 : n+1] {
			sum += v
		}
		count += n
	}
	if count > 0 {
		dc = (sum + count/2) / count
	}
	avg := func(a, b int) int { return (a + b + 1) >> 1 }
	filt := func(a, b, c int) int { return (a + 2*b + c + 2) >> 2 }
	verticalRight := func(x, y int, t, l []int) int {
		z := 2*x - y
		if z >= 0 {
			k := x - (y >> 1)
			if z%2 == 0 {
				return avg(t[k], t[k+1])
			}
			return filt(t[k-1], t[k], t[k+1])
		}
		if z == -1 {
			return filt(l[1], t[0], t[1])
		}
		k := y - 2*x
		return filt(l[k], l[k-1], l[k-2])
	}
	for j := 0; j < n; j++ {
		for i := 0; i < n; i++ {
			v := dc
			switch mode {
			case 0:
				v = top[i+1]
			case 1:
				v = left[j+1]
			case 3:
				k := i + j + 1
				v = filt(top[k], top[k+1], top[min(k+2, 2*n)])
			case 4:
				if i > j {
					k := i - j
					v = filt(top[k-1], top[k], top[k+1])
				} else if i < j {
					k := j - i
					v = filt(left[k-1], left[k], left[k+1])
				} else {
					v = filt(top[1], top[0], left[1])
				}
			case 5:
				v = verticalRight(i, j, top[:], left[:])
			case 6:
				v = verticalRight(j, i, left[:], top[:])
			case 7:
				k := i + (j >> 1) + 1
				if j%2 == 0 {
					v = avg(top[k], top[k+1])
				} else {
					v = filt(top[k], top[k+1], top[k+2])
				}
			case 8:
				k := j + (i >> 1) + 1
				if i%2 == 0 {
					v = avg(left[min(k, n)], left[min(k+1, n)])
				} else {
					v = filt(left[min(k, n)], left[min(k+1, n)], left[min(k+2, n)])
				}
			}
			data[(by+y+j)*stride+bx+x+i] = byte(v)
		}
	}
	return nil
}
func (w *workPicture) predictLarge(addr, plane, mode int, constrained bool) error {
	data, stride, x, y, n := w.plane(addr, plane)
	topOK := w.intraAvailable(addr, 0, -1, plane, 0, constrained)
	leftOK := w.intraAvailable(addr, -1, 0, plane, 0, constrained)
	cornerOK := w.intraAvailable(addr, -1, -1, plane, 0, constrained)
	var top, left [17]int
	if cornerOK {
		top[0] = int(data[(y-1)*stride+x-1])
		left[0] = top[0]
	}
	if topOK {
		for i := 0; i < n; i++ {
			top[i+1] = int(data[(y-1)*stride+x+i])
		}
	}
	if leftOK {
		for i := 0; i < n; i++ {
			left[i+1] = int(data[(y+i)*stride+x-1])
		}
	}
	if plane != 0 {
		if mode == 0 {
			mode = 2
		} else if mode == 2 {
			mode = 0
		}
	}
	if (mode == 0 && !topOK) || (mode == 1 && !leftOK) || (mode == 3 && (!topOK || !leftOK || !cornerOK)) {
		return fmt.Errorf("%w: unavailable intra references at macroblock %d plane %d mode %d", ErrMalformed, addr, plane, mode)
	}
	a, b, c := 0, 0, 0
	if mode == 3 {
		half := n / 2
		h, v := 0, 0
		for i := 1; i <= half; i++ {
			h += i * (top[half+i] - top[half-i])
			v += i * (left[half+i] - left[half-i])
		}
		a = 16 * (top[n] + left[n])
		b, c = (5*h+32)>>6, (5*v+32)>>6
		if n == 8 {
			b, c = (17*h+16)>>5, (17*v+16)>>5
		}
	}
	for j := 0; j < n; j++ {
		for i := 0; i < n; i++ {
			v := 128
			switch mode {
			case 0:
				v = top[i+1]
			case 1:
				v = left[j+1]
			case 3:
				v = clip((a+b*(i-n/2+1)+c*(j-n/2+1)+16)>>5, 0, 255)
			case 2:
				sum, count := 0, 0
				tx, ly, length := 0, 0, n
				useTop, useLeft := topOK, leftOK
				if plane != 0 {
					tx, ly, length = i/4*4, j/4*4, 4
					if tx > 0 && ly == 0 && topOK {
						useLeft = false
					}
					if ly > 0 && tx == 0 && leftOK {
						useTop = false
					}
				}
				if useTop {
					for k := 0; k < length; k++ {
						sum += top[tx+k+1]
					}
					count += length
				}
				if useLeft {
					for k := 0; k < length; k++ {
						sum += left[ly+k+1]
					}
					count += length
				}
				if count > 0 {
					v = (sum + count/2) / count
				}
			}
			data[(y+j)*stride+x+i] = byte(v)
		}
	}
	return nil
}
