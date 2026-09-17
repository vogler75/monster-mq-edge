// Package h264 implements H.264 bitstream decoding in Go without native codecs.
// Each Decoder belongs to one elementary stream and is not safe for concurrent
// use. Input to Decode is a complete access unit in decoding order; parameter
// sets supplied by SDP may also be passed on their own.
package h264

import (
	"fmt"
	"image"
)

// Config bounds allocations for untrusted streams. Zero values select defaults.
type Config struct {
	MaxPixels          int
	MaxAccessUnitBytes int
}

// Frame owns a decoded picture. Pixels are planar 8-bit YCbCr 4:2:0 samples in
// the bitstream's range and matrix, not necessarily JPEG's full-range BT.601.
// The caller may retain the picture; it must not modify the planes.
type Frame struct {
	Pixels             *image.YCbCr
	KeyFrame           bool
	FrameNum           int
	PictureOrderCount  int
	FullRange          bool
	MatrixCoefficients int
}

// Decoder retains parameter sets and reference pictures across access units.
type Decoder struct {
	cfg             Config
	sps             [32]*sequence
	pps             [256]*picture
	synced          bool
	refs            []*reference
	lastRefFrameNum int
	hasRefFrame     bool
	poc             pocState
	pending         []pendingPicture
	nextReferenceID uint64
}

func NewDecoder(cfg Config) *Decoder {
	if cfg.MaxPixels <= 0 {
		cfg.MaxPixels = 4096 * 2160
	}
	if cfg.MaxAccessUnitBytes <= 0 {
		cfg.MaxAccessUnitBytes = 16 << 20
	}
	return &Decoder{cfg: cfg}
}

// Reset discards all stream state, including out-of-band parameter sets.
func (d *Decoder) Reset() { cfg := d.cfg; *d = Decoder{cfg: cfg} }

// Decode reconstructs a complete access unit and returns pictures in display
// order. A call can return zero or several frames because pictures may need
// reordering; call Flush at end of stream. A parameter-set-only call returns no
// frames. On error no partial picture is returned. The next picture must be
// an IDR after a corrupt or unsupported picture, so references cannot drift.
func (d *Decoder) Decode(nalus [][]byte) (frames []*Frame, err error) {
	defer func() {
		if err != nil {
			d.Discontinuity()
		}
	}()
	size := 0
	if len(nalus) > 1024 {
		return nil, fmt.Errorf("%w: too many NAL units", ErrMalformed)
	}
	for _, n := range nalus {
		if len(n) == 0 || len(n) > d.cfg.MaxAccessUnitBytes-size {
			return nil, fmt.Errorf("%w: empty NAL or access unit exceeds byte limit", ErrMalformed)
		}
		size += len(n)
	}
	var w *workPicture
	sliceID := 0
	for _, nal := range nalus {
		if nal[0]&0x80 != 0 {
			return nil, fmt.Errorf("%w: forbidden_zero_bit", ErrMalformed)
		}
		typ := int(nal[0] & 31)
		switch typ {
		case 7:
			data, e := rbsp(nal)
			if e != nil {
				return nil, e
			}
			s, e := parseSPS(data, d.cfg.MaxPixels)
			if e != nil {
				return nil, e
			}
			d.sps[s.id] = s
		case 8:
			data, e := rbsp(nal)
			if e != nil {
				return nil, e
			}
			p, e := parsePPS(data)
			if e != nil {
				return nil, e
			}
			d.pps[p.id] = p
		case 1, 5:
			if typ == 1 && !d.synced {
				return nil, ErrNeedIDR
			}
			data, e := rbsp(nal)
			if e != nil {
				return nil, e
			}
			b := &bitReader{data: data}
			h, e := d.parseSlice(b, nal[0])
			if e != nil {
				return nil, e
			}
			if !h.idr && !d.synced {
				return nil, ErrNeedIDR
			}
			if w == nil {
				if !h.idr && h.refIDC != 0 && d.hasRefFrame && h.frameNum != (d.lastRefFrameNum+1)%(1<<h.sps.logFrame) {
					if h.sps.gapsAllowed {
						return nil, fmt.Errorf("%w: non-existing reference frame before frame_num %d", ErrUnsupported, h.frameNum)
					}
					return nil, fmt.Errorf("%w: missing reference frame before frame_num %d", ErrMalformed, h.frameNum)
				}
				w = newWorkPicture(h)
			} else if h.frameNum != w.header.frameNum || h.idr != w.header.idr || h.idrID != w.header.idrID || h.pocLSB != w.header.pocLSB || h.deltaPOC != w.header.deltaPOC || h.refIDC != w.header.refIDC || h.pps != w.header.pps || h.sps != w.header.sps {
				return nil, fmt.Errorf("%w: multiple pictures in one access unit", ErrMalformed)
			}
			if h.first != w.count {
				return nil, fmt.Errorf("%w: nonconsecutive or missing slice (first_mb %d, expected %d)", ErrMalformed, h.first, w.count)
			}
			sliceID++
			if e = w.decodeSlice(b, h, sliceID); e != nil {
				return nil, e
			}
		case 6, 9, 10, 11, 12: // Non-VCL messages do not change reconstructed samples.
		default:
			return nil, fmt.Errorf("%w: NAL unit type %d", ErrUnsupported, typ)
		}
	}
	if w == nil {
		return nil, nil
	}
	if w.count != len(w.mbs) {
		return nil, fmt.Errorf("%w: incomplete picture (%d/%d macroblocks)", ErrMalformed, w.count, len(w.mbs))
	}
	// Deblocking is applied after the whole picture to preserve intra neighbours.
	w.deblock()
	if w.header.idr || w.header.mmco5() {
		if w.header.discardPrior {
			d.pending = nil
		} else {
			frames = d.drain(0)
		}
	}
	if err := d.storeReference(w); err != nil {
		return nil, err
	}
	d.synced = true
	d.commitPOC(w.header)
	s := w.header.sps
	r := image.Rect(2*s.cropLeft, 2*s.cropTop, s.widthMB*16-2*s.cropRight, s.heightMB*16-2*s.cropBottom)
	cropped := w.img.SubImage(r).(*image.YCbCr)
	cropped.Rect = image.Rect(0, 0, r.Dx(), r.Dy())
	f := &Frame{Pixels: cropped, KeyFrame: w.header.idr, FrameNum: w.header.frameNum, PictureOrderCount: w.header.poc, FullRange: s.fullRange, MatrixCoefficients: s.matrix}
	d.pending = append(d.pending, pendingPicture{frame: f, img: w.img})
	frames = append(frames, d.drain(s.reorder)...)
	for len(d.pending) > 0 && d.bufferedPictures() > max(1, s.buffering) {
		frames = append(frames, d.drain(len(d.pending)-1)...)
	}
	return frames, nil
}

type sliceHeader struct {
	scaling                             scalingLists
	cabacInit                           int
	numRefs                             [2]int
	list                                [2][]*reference
	weights                             [2][]weight
	marking                             []marking
	adaptive, longIDR                   bool
	discardPrior, directSpatial         bool
	deltaPOC                            [2]int
	poc, topPOC, bottomPOC              int
	pocMSB, frameOffset                 int64
	first, typ, frameNum, idrID, pocLSB int
	idr                                 bool
	refIDC                              int
	sps                                 *sequence
	pps                                 *picture
	qp, disableDeblock, alpha, beta     int
}

func (d *Decoder) parseSlice(b *bitReader, nal byte) (*sliceHeader, error) {
	h := &sliceHeader{first: b.rangeUE(1 << 20), typ: b.rangeUE(9) % 5, idr: nal&31 == 5, refIDC: int(nal>>5) & 3}
	pid := b.rangeUE(255)
	if b.err != nil {
		return nil, b.err
	}
	h.pps = d.pps[pid]
	if h.pps == nil {
		return nil, fmt.Errorf("%w: missing PPS %d", ErrMalformed, pid)
	}
	p := h.pps
	h.sps = d.sps[p.spsID]
	if h.sps == nil {
		return nil, fmt.Errorf("%w: missing SPS %d", ErrMalformed, p.spsID)
	}
	s := h.sps
	h.scaling = resolveScaling(s.scaling, p.scaling)
	h.frameNum = b.bits(s.logFrame)
	if h.idr {
		h.idrID = b.rangeUE(65535)
		if h.frameNum != 0 || h.refIDC == 0 {
			b.fail("invalid IDR frame number or reference flag")
		}
	}
	switch s.pocType {
	case 0:
		h.pocLSB = b.bits(s.logPOC)
		if p.bottomPOC {
			h.deltaPOC[1] = b.se()
		}
	case 1:
		if !s.deltaAlwaysZero {
			h.deltaPOC[0] = b.se()
			if p.bottomPOC {
				h.deltaPOC[1] = b.se()
			}
		}
	}
	if p.redundant && b.ue() != 0 {
		return nil, fmt.Errorf("%w: redundant picture", ErrUnsupported)
	}
	if h.typ != 2 && h.typ != 0 && h.typ != 1 {
		return nil, fmt.Errorf("%w: slice type %d", ErrUnsupported, h.typ)
	}
	if err := d.derivePOC(h); err != nil {
		return nil, err
	}
	if err := d.parseReferences(b, h); err != nil {
		return nil, err
	}
	if p.cabac && h.typ != 2 {
		h.cabacInit = b.rangeUE(2)
	}
	h.qp = p.qp + b.se()
	if h.qp < 0 || h.qp > 51 {
		b.fail("slice QP outside [0,51]")
	}
	if p.deblock {
		h.disableDeblock = b.rangeUE(2)
		if h.disableDeblock != 1 {
			h.alpha = b.rangeSE(-6, 6) * 2
			h.beta = b.rangeSE(-6, 6) * 2
		}
	}
	if b.err != nil {
		return nil, b.err
	}

	return h, nil
}

type macroblock struct {
	direct                      bool
	directBlock                 [16]bool
	transform8, smallInter      bool
	skip                        bool
	mvd                         [2][16]motion
	chromaMode, cbp             int
	dcNZ                        [3]bool
	mv                          [2][16]motion
	refIndex                    [2][16]int
	ref                         [2][16]*reference
	motionSet                   [2][16]bool
	sliceID                     int
	intra, i16, pcm, done       bool
	mode                        [16]int
	nz                          [24]int
	qp                          [3]int
	disableDeblock, alpha, beta int
}
type workPicture struct {
	img    *image.YCbCr
	header *sliceHeader
	mbs    []macroblock
	count  int
}

func newWorkPicture(h *sliceHeader) *workPicture {
	s := h.sps
	return &workPicture{img: image.NewYCbCr(image.Rect(0, 0, s.widthMB*16, s.heightMB*16), image.YCbCrSubsampleRatio420), header: h, mbs: make([]macroblock, s.widthMB*s.heightMB)}
}
func blockXY(i int) (int, int) { return (i/4%2)*8 + (i%2)*4, (i/8)*8 + (i/2%2)*4 }
func blockIndex(x, y int) int  { return (y/8*2+x/8)*4 + (y%8/4)*2 + x%8/4 }

// neighbour returns only macroblocks in the current slice. Pixels across slice
// boundaries remain available to deblocking, but not to intra prediction.
func (w *workPicture) neighbour(addr, x, y, plane, sliceID int) (*macroblock, int) {
	n := 16
	if plane != 0 {
		n = 8
	}
	mx, my := addr%w.header.sps.widthMB, addr/w.header.sps.widthMB
	if x < 0 {
		mx--
		x += n
	}
	if x >= n {
		mx++
		x -= n
	}
	if y < 0 {
		my--
		y += n
	}
	if y >= n {
		my++
		y -= n
	}
	if mx < 0 || mx >= w.header.sps.widthMB || my < 0 || my >= w.header.sps.heightMB {
		return nil, 0
	}
	a := my*w.header.sps.widthMB + mx
	m := &w.mbs[a]
	if m.sliceID != sliceID || (a != addr && !m.done) {
		return nil, 0
	}
	i := blockIndex(x, y)
	if plane != 0 {
		i = 16 + (plane-1)*4 + y/4*2 + x/4
	}
	return m, i
}
func (w *workPicture) nc(addr, idx, sliceID int) int {
	plane, x, y := 0, 0, 0
	if idx < 16 {
		x, y = blockXY(idx)
	} else {
		plane = (idx-16)/4 + 1
		x = (idx % 4 % 2) * 4
		y = (idx % 4 / 2) * 4
	}
	a, ai := w.neighbour(addr, x-1, y, plane, sliceID)
	b, bi := w.neighbour(addr, x, y-1, plane, sliceID)
	if a != nil && b != nil {
		return (a.nz[ai] + b.nz[bi] + 1) / 2
	}
	if a != nil {
		return a.nz[ai]
	}
	if b != nil {
		return b.nz[bi]
	}
	return 0
}

var intraCBP = [48]int{47, 31, 15, 0, 23, 27, 29, 30, 7, 11, 13, 14, 39, 43, 45, 46, 16, 3, 5, 10, 12, 19, 21, 26, 28, 35, 37, 42, 44, 1, 2, 4, 8, 17, 18, 20, 24, 6, 9, 22, 25, 32, 33, 34, 36, 40, 38, 41}

func (w *workPicture) decodeSlice(b *bitReader, h *sliceHeader, sliceID int) error {
	qp := h.qp
	e := &entropyReader{b: b, w: w, h: h}
	if h.pps.cabac {
		e.c = newCABAC(b, h)
	}
	ended := false
	for b.err == nil && (e.c != nil || b.more()) && w.count < len(w.mbs) {
		if h.typ != 2 && e.c == nil {
			skip := b.rangeUE(len(w.mbs) - w.count)
			for i := 0; i < skip && b.err == nil; i++ {
				addr := w.count
				m := &w.mbs[addr]
				m.sliceID = sliceID
				m.disableDeblock, m.alpha, m.beta = h.disableDeblock, h.alpha, h.beta
				m.qp = [3]int{qp, qpc(qp, h.pps.chromaOffset), qpc(qp, h.pps.secondChromaOffset)}
				if err := w.predictSkip(addr, h); err != nil {
					return err
				}
				m.done = true
				w.count++
			}
			if b.err != nil {
				return b.err
			}
			if !b.more() {
				break
			}
			if w.count == len(w.mbs) {
				b.fail("macroblocks beyond picture")
				break
			}
		}
		addr := w.count
		m := &w.mbs[addr]
		m.sliceID = sliceID
		m.intra = h.typ == 2
		m.disableDeblock, m.alpha, m.beta = h.disableDeblock, h.alpha, h.beta
		if h.typ != 2 && e.c != nil && e.skip(addr) {
			m.skip = true
			m.qp = [3]int{qp, qpc(qp, h.pps.chromaOffset), qpc(qp, h.pps.secondChromaOffset)}
			if err := w.predictSkip(addr, h); err != nil {
				return err
			}
			e.prevDelta = 0
			m.done = true
			w.count++
			if e.c.terminate() {
				ended = true
				break
			}
			continue
		}
		typ := e.mbType(addr)
		if h.typ == 1 && typ >= 23 {
			m.intra = true
			typ -= 23
		}
		if h.typ == 0 && typ >= 5 {
			m.intra = true
			typ -= 5
		}
		if m.intra && typ > 25 {
			b.fail("invalid intra macroblock type")
			return b.err
		}
		if m.intra && typ == 25 {
			for b.pos%8 != 0 && b.err == nil {
				if b.bits(1) != 0 {
					b.fail("nonzero PCM alignment bit")
				}
			}
			w.readPCM(b, addr)
			e.prevDelta = 0
			if e.c != nil {
				e.c.restart()
			}
			m.pcm = true
			m.qp = [3]int{0, 0, 0}
			for i := range m.nz {
				m.nz[i] = 16
			}
		} else {
			i16 := m.intra && typ != 0
			m.i16 = i16
			cbp, mode16 := 0, 0
			if i16 {
				mode16 = (typ - 1) % 4
				cbp = ((typ - 1) / 4 % 3) * 16
				if typ >= 13 {
					cbp += 15
				}
			} else if m.intra {
				if h.pps.transform8 {
					m.transform8 = e.transform8(addr)
				}
				step := 1
				if m.transform8 {
					step = 4
				}
				for i := 0; i < 16; i += step {
					x, y := blockXY(i)
					a, ai := w.neighbour(addr, x-1, y, 0, sliceID)
					c, ci := w.neighbour(addr, x, y-1, 0, sliceID)
					pred := 2
					if a != nil && c != nil && !(h.pps.constrained && (!a.intra || !c.intra)) {
						am, cm := 2, 2
						if a.intra && !a.i16 && !a.pcm {
							am = a.mode[ai]
						}
						if c.intra && !c.i16 && !c.pcm {
							cm = c.mode[ci]
						}
						pred = min(am, cm)
					}
					mode := e.mode(pred)
					for j := 0; j < step; j++ {
						m.mode[i+j] = mode
					}
				}
			}
			if !m.intra {
				if err := w.readInter(e, addr, typ, h); err != nil {
					return err
				}

			}
			cmode := 0
			if m.intra {
				cmode = e.chromaMode(addr)
			}
			m.chromaMode = cmode
			if !i16 {
				cbp = e.cbp(addr, m.intra)
			}
			m.cbp = cbp
			if !m.intra && cbp&15 != 0 && h.pps.transform8 && !m.smallInter {
				m.transform8 = e.transform8(addr)
			}
			var coeff8 [4][64]int
			var coeff [24][16]int
			var dc [16]int
			if cbp != 0 || i16 {
				delta := e.delta()
				qp = (qp + delta + 52) % 52
				if i16 {
					var n int
					dc, n = e.coeff(addr, 0, 0, 16)
					m.dcNZ[0] = n != 0
				}
				if m.transform8 {
					for group := 0; group < 4; group++ {
						if cbp&(1<<group) != 0 {
							coeff8[group] = e.coeff8(addr, group)
						}
					}
				} else {
					for i := 0; i < 16; i++ {
						if cbp&(1<<(i/4)) == 0 {
							continue
						}
						maxc := 16
						if i16 {
							maxc = 15
						}
						cat := 2
						if i16 {
							cat = 1
						}
						c, n := e.coeff(addr, i, cat, maxc)
						m.nz[i] = n
						if i16 {
							copy(coeff[i][1:], c[:15])
						} else {
							coeff[i] = c
						}
					}
				}

				var cdc [2][16]int
				if cbp/16 > 0 {
					for plane := range cdc {
						var n int
						cdc[plane], n = e.coeff(addr, 16+4*plane, 3, 4)
						m.dcNZ[plane+1] = n != 0
					}
				}
				if cbp/16 == 2 {
					for i := 16; i < 24; i++ {
						c, n := e.coeff(addr, i, 4, 15)
						copy(coeff[i][1:], c[:15])
						m.nz[i] = n
					}
				}
				m.qp = [3]int{qp, qpc(qp, h.pps.chromaOffset), qpc(qp, h.pps.secondChromaOffset)}
				for plane := 1; plane <= 2; plane++ {
					c := chromaDC(cdc[plane-1], m.qp[plane], int(h.scaling[scalingIndex(m, plane)][0]))
					for i, v := range c {
						coeff[16+(plane-1)*4+i][0] = v
					}
				}
			} else {
				e.prevDelta = 0
				m.qp = [3]int{qp, qpc(qp, h.pps.chromaOffset), qpc(qp, h.pps.secondChromaOffset)}
			}
			if b.err != nil {
				return fmt.Errorf("macroblock %d: %w", addr, b.err)
			}
			if m.transform8 {
				if err := w.reconstruct8(addr, coeff8, h.pps.constrained); err != nil {
					return err
				}
			}
			if m.intra {
				if err := w.reconstructIntra(addr, mode16, cmode, coeff, dc, h.pps.constrained); err != nil {
					return err
				}
			} else {
				for i := 0; i < 16 && !m.transform8; i++ {
					x, y := blockXY(i)
					if err := w.addResidual4(addr, 0, x, y, coeff[i], qp, nil); err != nil {
						return err
					}
				}
				for plane := 1; plane < 3; plane++ {
					for i := 0; i < 4; i++ {
						c := coeff[16+(plane-1)*4+i]
						if err := w.addResidual4(addr, plane, i%2*4, i/2*4, c, m.qp[plane], &c[0]); err != nil {
							return err
						}
					}
				}
			}
		}
		if b.err != nil {
			return fmt.Errorf("macroblock %d: %w", addr, b.err)
		}
		m.done = true
		w.count++
		if e.c != nil && e.c.terminate() {
			ended = true
			break
		}
	}
	if e.c == nil {
		b.trailing()
	} else if !ended {
		b.fail("missing CABAC end_of_slice_flag")
	} else {
		e.c.finish()
	}
	return b.err
}
func (w *workPicture) readPCM(b *bitReader, addr int) {
	for plane := 0; plane < 3; plane++ {
		data, stride, x, y, n := w.plane(addr, plane)
		for j := 0; j < n; j++ {
			for i := 0; i < n; i++ {
				data[(y+j)*stride+x+i] = byte(b.bits(8))
			}
		}
	}
}
func (w *workPicture) plane(addr, plane int) ([]byte, int, int, int, int) {
	x, y := addr%w.header.sps.widthMB*16, addr/w.header.sps.widthMB*16
	if plane == 0 {
		return w.img.Y, w.img.YStride, x, y, 16
	}
	data := w.img.Cb
	if plane == 2 {
		data = w.img.Cr
	}
	return data, w.img.CStride, x / 2, y / 2, 8
}

var interCBP = [48]int{0, 16, 1, 2, 4, 8, 32, 3, 5, 10, 12, 15, 47, 7, 11, 13, 14, 6, 9, 31, 35, 37, 42, 44, 33, 34, 36, 40, 39, 43, 45, 46, 17, 18, 20, 24, 19, 21, 26, 28, 23, 27, 29, 30, 22, 25, 38, 41}
