package integration

import (
	"bytes"
	"errors"
	"testing"

	"monstermq.io/edge/pkg/h264"
)

// Minimal progressive I_PCM streams isolate ordering from entropy and motion
// decoding. Syntax is written directly from clauses 7.3.2, 7.3.3 and 7.3.5.
type h264Bits struct {
	data []byte
	pos  int
}

func (b *h264Bits) bits(value, n int) {
	for i := n - 1; i >= 0; i-- {
		if b.pos%8 == 0 {
			b.data = append(b.data, 0)
		}
		b.data[b.pos/8] |= byte((value>>i)&1) << uint(7-b.pos%8)
		b.pos++
	}
}
func (b *h264Bits) ue(v int) {
	n := 0
	for x := v + 1; x > 1; x >>= 1 {
		n++
	}
	b.bits(0, n)
	b.bits(v+1, n+1)
}
func (b *h264Bits) se(v int) {
	if v > 0 {
		b.ue(2*v - 1)
	} else {
		b.ue(-2 * v)
	}
}
func (b *h264Bits) nal(header byte) []byte {
	b.bits(1, 1)
	for b.pos%8 != 0 {
		b.bits(0, 1)
	}
	out := []byte{header}
	zeros := 0
	for _, v := range b.data {
		if zeros == 2 && v <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}
func h264PCMParams(pocType int, gaps bool) [][]byte {
	s := &h264Bits{}
	s.bits(66, 8)
	s.bits(0, 8)
	s.bits(10, 8)
	s.ue(0)
	s.ue(0)
	s.ue(pocType)
	switch pocType {
	case 0:
		s.ue(0)
	case 1:
		s.bits(0, 1)
		s.se(-1)
		s.se(0)
		s.ue(1)
		s.se(2)
	}
	s.ue(1)
	if gaps {
		s.bits(1, 1)
	} else {
		s.bits(0, 1)
	}
	s.ue(0)
	s.ue(0)
	s.bits(1, 1)
	s.bits(1, 1)
	s.bits(0, 1)
	s.bits(1, 1) // VUI with explicit reordering/buffering limits.
	s.bits(0, 8)
	s.bits(1, 1)
	s.bits(1, 1)
	s.ue(0)
	s.ue(0)
	s.ue(10)
	s.ue(10)
	s.ue(2)
	s.ue(4)
	p := &h264Bits{}
	p.ue(0)
	p.ue(0)
	p.bits(0, 2)
	p.ue(0)
	p.ue(0)
	p.ue(0)
	p.bits(0, 3)
	p.se(0)
	p.se(0)
	p.se(0)
	p.bits(0, 3)
	return [][]byte{s.nal(0x67), p.nal(0x68)}
}

type pcmPicture struct {
	num, order, delta, value int
	ref, idr, reset, discard bool
}

func h264PCMPicture(pocType int, p pcmPicture) []byte {
	b := &h264Bits{}
	b.ue(0)
	b.ue(2)
	b.ue(0)
	b.bits(p.num, 4)
	if p.idr {
		b.ue(0)
	}
	switch pocType {
	case 0:
		b.bits(p.order&15, 4)
	case 1:
		b.se(p.delta)
	}
	if p.ref {
		if p.idr {
			if p.discard {
				b.bits(1, 1)
			} else {
				b.bits(0, 1)
			}
			b.bits(0, 1)
		} else if p.reset {
			b.bits(1, 1)
			b.ue(5)
			b.ue(0)
		} else {
			b.bits(0, 1)
		}
	}
	b.se(0)
	b.ue(25)
	for b.pos%8 != 0 {
		b.bits(0, 1)
	}
	for i := 0; i < 384; i++ {
		b.bits(p.value, 8)
	}
	header := byte(1)
	if p.ref {
		header |= 0x60
	}
	if p.idr {
		header = 0x65
	}
	return b.nal(header)
}
func TestH264PictureOrdering(t *testing.T) {
	for _, pocType := range []int{0, 1, 2} {
		t.Run(string(rune('0'+pocType)), func(t *testing.T) {
			dec := h264.NewDecoder(h264.Config{})
			if _, err := dec.Decode(h264PCMParams(pocType, false)); err != nil {
				t.Fatal(err)
			}
			pics := []pcmPicture{{num: 0, order: 0, value: 0, ref: true, idr: true}}
			if pocType == 2 {
				for i := 1; i <= 20; i++ {
					pics = append(pics, pcmPicture{num: i % 16, order: 2 * i, value: i, ref: true})
				}
			} else {
				for group := 1; group <= 4; group++ {
					order := group * 6
					pics = append(pics, pcmPicture{num: group, order: order, delta: order - 2*group, value: order / 2, ref: true})
					for _, offset := range []int{4, 2} {
						p := order - offset
						pics = append(pics, pcmPicture{num: group + 1, order: p, delta: p - (2*group - 1), value: p / 2})
					}
				}
			}
			var values []byte
			add := func(frames []*h264.Frame) {
				for _, f := range frames {
					values = append(values, f.Pixels.Y[0])
					if f.PictureOrderCount != 2*int(f.Pixels.Y[0]) {
						t.Fatalf("value %d has POC %d", f.Pixels.Y[0], f.PictureOrderCount)
					}
				}
			}
			for i, p := range pics {
				frames, err := dec.Decode([][]byte{h264PCMPicture(pocType, p)})
				if err != nil {
					t.Fatalf("picture %d: %v", i, err)
				}
				add(frames)
			}
			if len(values) >= len(pics) {
				t.Fatal("fixture did not exercise delayed output")
			}
			add(dec.Flush())
			want := make([]byte, len(pics))
			for i := range want {
				want[i] = byte(i)
			}
			if !bytes.Equal(values, want) {
				t.Fatalf("display values %v want %v", values, want)
			}
			if len(dec.Flush()) != 0 {
				t.Fatal("flush returned pictures twice")
			}
		})
	}
}

func TestH264OrderingReset(t *testing.T) {
	for _, action := range []string{"IDR flush", "IDR discard", "MMCO5", "discontinuity"} {
		t.Run(action, func(t *testing.T) {
			dec := h264.NewDecoder(h264.Config{})
			if _, err := dec.Decode(h264PCMParams(0, false)); err != nil {
				t.Fatal(err)
			}
			for _, p := range []pcmPicture{{ref: true, idr: true, value: 10}, {num: 1, order: 4, ref: true, value: 20}} {
				frames, err := dec.Decode([][]byte{h264PCMPicture(0, p)})
				if err != nil || len(frames) != 0 {
					t.Fatalf("prime: %v %d", err, len(frames))
				}
			}
			p := pcmPicture{ref: true, idr: true, value: 30}
			want := []byte{10, 20, 30}
			if action == "IDR discard" {
				p.discard = true
				want = []byte{30}
			}
			if action == "discontinuity" {
				dec.Discontinuity()
				want = []byte{30}
			}
			if action == "MMCO5" {
				p = pcmPicture{num: 2, order: 8, ref: true, reset: true, value: 30}
			}
			frames, err := dec.Decode([][]byte{h264PCMPicture(0, p)})
			if err != nil {
				t.Fatal(err)
			}
			frames = append(frames, dec.Flush()...)
			var got []byte
			for _, f := range frames {
				got = append(got, f.Pixels.Y[0])
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
			if frames[len(frames)-1].PictureOrderCount != 0 {
				t.Fatal("reset picture POC was not normalized")
			}
			next, err := dec.Decode([][]byte{h264PCMPicture(0, pcmPicture{num: 1, order: 2, ref: true, value: 40})})
			if err != nil {
				t.Fatal(err)
			}
			next = append(next, dec.Flush()...)
			if len(next) != 1 || next[0].PictureOrderCount != 2 {
				t.Fatal("POC state was not reset")
			}
		})
	}
}

func TestH264ExplicitWeights(t *testing.T) {
	for _, bSlice := range []bool{false, true} {
		dec := h264.NewDecoder(h264.Config{})
		params := h264PCMParams(0, false)
		p := &h264Bits{}
		p.ue(0)
		p.ue(0)
		p.bits(0, 2)
		p.ue(0)
		p.ue(0)
		p.ue(0)
		p.bits(1, 1)
		p.bits(1, 2)
		p.se(0)
		p.se(0)
		p.se(0)
		p.bits(0, 3)
		params[1] = p.nal(0x68)
		// Retain both references for B prediction (Main profile).
		s := &h264Bits{}
		s.bits(77, 8)
		s.bits(0, 8)
		s.bits(10, 8)
		s.ue(0)
		s.ue(0)
		s.ue(0)
		s.ue(0)
		s.ue(2)
		s.bits(0, 1)
		s.ue(0)
		s.ue(0)
		s.bits(1, 1)
		s.bits(1, 1)
		s.bits(0, 1)
		s.bits(0, 1)
		params[0] = s.nal(0x67)
		if _, err := dec.Decode(params); err != nil {
			t.Fatal(err)
		}
		if _, err := dec.Decode([][]byte{h264PCMPicture(0, pcmPicture{idr: true, ref: true, value: 40})}); err != nil {
			t.Fatal(err)
		}
		if bSlice {
			if _, err := dec.Decode([][]byte{h264PCMPicture(0, pcmPicture{num: 1, order: 4, ref: true, value: 80})}); err != nil {
				t.Fatal(err)
			}
		}
		b := &h264Bits{}
		b.ue(0)
		if bSlice {
			b.ue(1)
		} else {
			b.ue(0)
		}
		b.ue(0)
		num := 1
		if bSlice {
			num = 2
		}
		b.bits(num, 4)
		b.bits(2, 4)
		if bSlice {
			b.bits(0, 1)
		} // Temporal direct, using the two constant pictures.
		b.bits(0, 1)
		b.bits(0, 1)
		if bSlice {
			b.bits(0, 1)
		}
		b.ue(2)
		b.ue(1)
		b.bits(1, 1)
		b.se(3)
		b.se(5)
		b.bits(1, 1)
		b.se(2)
		b.se(-6)
		b.se(1)
		b.se(7)
		if bSlice {
			b.bits(1, 1)
			b.se(5)
			b.se(-9)
			b.bits(1, 1)
			b.se(2)
			b.se(2)
			b.se(3)
			b.se(-3)
		}
		b.se(0)
		b.ue(1) // One skipped macroblock, no residuals.
		frames, err := dec.Decode([][]byte{b.nal(0x01)})
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, dec.Flush()...)
		found := false
		for _, f := range frames {
			if f.FrameNum != num {
				continue
			}
			found = true
			want := [3]byte{35, 34, 27}
			if bSlice {
				want = [3]byte{63, 58, 72}
			}
			for i, plane := range [][]byte{f.Pixels.Y, f.Pixels.Cb, f.Pixels.Cr} {
				for _, v := range plane {
					if v != want[i] {
						t.Fatalf("B=%v plane %d got %d want %d", bSlice, i, v, want[i])
					}
				}
			}
		}
		if !found {
			t.Fatal("weighted picture missing")
		}
	}
}

func TestH264PermittedFrameNumberGaps(t *testing.T) {
	for _, allow := range []bool{false, true} {
		dec := h264.NewDecoder(h264.Config{})
		if _, err := dec.Decode(h264PCMParams(0, allow)); err != nil {
			t.Fatal(err)
		}
		for _, p := range []pcmPicture{{ref: true, idr: true}, {num: 1, order: 2, ref: true}} {
			if _, err := dec.Decode([][]byte{h264PCMPicture(0, p)}); err != nil {
				t.Fatal(err)
			}
		}
		// Declaring permission is supported; synthesizing an absent reference is
		// still explicit, rather than silently using a different reference picture.
		_, err := dec.Decode([][]byte{h264PCMPicture(0, pcmPicture{num: 3, order: 6, ref: true})})
		want := h264.ErrMalformed
		if allow {
			want = h264.ErrUnsupported
		}
		if !errors.Is(err, want) {
			t.Fatalf("allow=%v error=%v", allow, err)
		}
	}
}
