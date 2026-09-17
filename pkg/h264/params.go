package h264

import "fmt"

type sequence struct {
	scaling                                       scalingSyntax
	id, profile, level, logFrame, pocType, logPOC int
	deltaAlwaysZero                               bool
	offsetNonRef, offsetTopBottom                 int
	offsets                                       []int
	refs, widthMB, heightMB                       int
	cropLeft, cropRight, cropTop, cropBottom      int
	fullRange                                     bool
	matrix                                        int
	direct8                                       bool
	gapsAllowed                                   bool
	reorder, buffering                            int
}
type picture struct {
	scaling                                     scalingSyntax
	id, spsID                                   int
	cabac, bottomPOC                            bool
	refs                                        [2]int
	weighted                                    bool
	weightedB                                   int
	qp, chromaOffset, secondChromaOffset        int
	deblock, constrained, redundant, transform8 bool
}

func parseSPS(data []byte, maxPixels int) (*sequence, error) {
	b := &bitReader{data: data}
	s := &sequence{profile: b.bits(8), matrix: 2}
	constraints := b.bits(8)
	if constraints&3 != 0 {
		b.fail("nonzero SPS reserved bits")
	}
	s.level = b.bits(8)
	s.id = b.rangeUE(31)
	switch s.profile {
	case 66, 77, 88:
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma := b.rangeUE(3)
		if chroma != 1 {
			return nil, fmt.Errorf("%w: chroma_format_idc %d", ErrUnsupported, chroma)
		}
		if b.ue() != 0 || b.ue() != 0 {
			return nil, fmt.Errorf("%w: bit depth above 8", ErrUnsupported)
		}
		if b.flag() {
			return nil, fmt.Errorf("%w: transform bypass", ErrUnsupported)
		}
		s.scaling = parseScaling(b, 8)
	default:
		return nil, fmt.Errorf("%w: profile_idc %d", ErrUnsupported, s.profile)
	}
	s.logFrame = b.rangeUE(12) + 4
	s.pocType = b.rangeUE(2)
	switch s.pocType {
	case 0:
		s.logPOC = b.rangeUE(12) + 4
	case 1:
		s.deltaAlwaysZero = b.flag()
		s.offsetNonRef = b.se()
		s.offsetTopBottom = b.se()
		n := b.rangeUE(255)
		for i := 0; i < n && b.err == nil; i++ {
			s.offsets = append(s.offsets, b.se())
		}
	}
	s.refs = b.rangeUE(16)
	s.gapsAllowed = b.flag()
	s.widthMB = b.rangeUE(4095) + 1
	s.heightMB = b.rangeUE(4095) + 1
	if !b.flag() {
		return nil, fmt.Errorf("%w: field or MBAFF pictures", ErrUnsupported)
	}
	s.direct8 = b.flag()
	// Annex A, Table A-1; Annex E supplies these defaults when VUI does not.
	dpb := map[int]int{10: 396, 11: 900, 12: 2376, 13: 2376, 20: 2376, 21: 4752, 22: 8100, 30: 8100, 31: 18000, 32: 20480, 40: 32768, 41: 32768, 42: 34816, 50: 110400, 51: 184320, 52: 184320, 60: 696320, 61: 696320, 62: 696320}[s.level]
	if s.level == 11 && constraints&16 != 0 && (s.profile == 66 || s.profile == 77 || s.profile == 88) {
		dpb = 396
	}
	if dpb == 0 {
		return nil, fmt.Errorf("%w: level_idc %d", ErrUnsupported, s.level)
	}
	s.buffering = min(16, dpb/(s.widthMB*s.heightMB))
	s.reorder = s.buffering
	if constraints&16 != 0 && (s.profile == 44 || s.profile == 86 || s.profile == 100 || s.profile == 110 || s.profile == 122 || s.profile == 244) {
		s.reorder = 0
		s.buffering = 0
	}
	if b.flag() {
		s.cropLeft = b.rangeUE(32767)
		s.cropRight = b.rangeUE(32767)
		s.cropTop = b.rangeUE(32767)
		s.cropBottom = b.rangeUE(32767)
	}
	if b.flag() {
		parseVUI(b, s)
	}
	b.trailing()
	if b.err != nil {
		return nil, b.err
	}
	if s.buffering < s.refs || s.reorder > s.buffering || s.buffering > min(16, dpb/(s.widthMB*s.heightMB)) {
		return nil, fmt.Errorf("%w: invalid decoded picture buffer limits", ErrMalformed)
	}
	w, h := s.widthMB*16, s.heightMB*16
	if int64(w)*int64(h) > int64(maxPixels) {
		return nil, fmt.Errorf("%w: coded dimensions %dx%d exceed pixel limit %d", ErrUnsupported, w, h, maxPixels)
	}
	if 2*(s.cropLeft+s.cropRight) >= w || 2*(s.cropTop+s.cropBottom) >= h {
		return nil, fmt.Errorf("%w: invalid cropping", ErrMalformed)
	}
	return s, nil
}
func parseVUI(b *bitReader, s *sequence) {
	if b.flag() {
		if b.bits(8) == 255 {
			b.bits(16)
			b.bits(16)
		}
	}
	if b.flag() {
		b.flag()
	}
	if b.flag() {
		b.bits(3)
		s.fullRange = b.flag()
		if b.flag() {
			b.bits(8)
			b.bits(8)
			s.matrix = b.bits(8)
		}
	}
	if b.flag() {
		b.rangeUE(5)
		b.rangeUE(5)
	}
	if b.flag() {
		b.bits(16)
		b.bits(16)
		b.bits(16)
		b.bits(16)
		b.flag()
	}
	nal, vcl := b.flag(), false
	if nal {
		parseHRD(b)
	}
	vcl = b.flag()
	if vcl {
		parseHRD(b)
	}
	if nal || vcl {
		b.flag()
	}
	b.flag()
	if b.flag() {
		b.flag()
		b.ue()
		b.ue()
		b.ue()
		b.ue()
		s.reorder = b.rangeUE(16)
		s.buffering = b.rangeUE(16)
	}
}
func parseHRD(b *bitReader) {
	n := b.rangeUE(31) + 1
	b.bits(4)
	b.bits(4)
	for i := 0; i < n && b.err == nil; i++ {
		b.ue()
		b.ue()
		b.flag()
	}
	b.bits(5)
	b.bits(5)
	b.bits(5)
	b.bits(5)
}
func parsePPS(data []byte) (*picture, error) {
	b := &bitReader{data: data}
	p := &picture{id: b.rangeUE(255), spsID: b.rangeUE(31)}
	p.cabac = b.flag()
	p.bottomPOC = b.flag()
	if b.ue() != 0 {
		return nil, fmt.Errorf("%w: slice groups", ErrUnsupported)
	}
	p.refs[0] = b.rangeUE(31) + 1
	p.refs[1] = b.rangeUE(31) + 1
	p.weighted = b.flag()
	p.weightedB = b.bits(2)
	if p.weightedB > 2 {
		b.fail("reserved weighted_bipred_idc")
	}
	p.qp = b.rangeSE(-26, 25) + 26
	b.rangeSE(-26, 25)
	p.chromaOffset = b.rangeSE(-12, 12)
	p.secondChromaOffset = p.chromaOffset
	p.deblock = b.flag()
	p.constrained = b.flag()
	p.redundant = b.flag()
	if b.more() {
		p.transform8 = b.flag()
		p.scaling = parseScaling(b, 6+2*boolInt(p.transform8))
		p.secondChromaOffset = b.rangeSE(-12, 12)
	}
	b.trailing()
	if b.err != nil {
		return nil, b.err
	}
	return p, nil
}
