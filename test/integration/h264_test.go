package integration

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"monstermq.io/edge/pkg/h264"
)

func TestH264ReferencePixels(t *testing.T) {
	files, err := filepath.Glob("testdata/h264/*.264")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no H.264 fixtures")
	}
	for _, path := range files {
		name := strings.TrimSuffix(filepath.Base(path), ".264")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/h264/" + name + ".264")
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile("testdata/h264/" + name + ".yuv")
			if err != nil {
				t.Fatal(err)
			}
			nals, err := h264.SplitAnnexB(raw)
			if err != nil {
				t.Fatal(err)
			}
			dec := h264.NewDecoder(h264.Config{})
			var got []byte
			var unit [][]byte
			count := 0
			appendFrames := func(frames []*h264.Frame) {
				for _, f := range frames {
					img := f.Pixels
					for y := 0; y < img.Rect.Dy(); y++ {
						i := y * img.YStride
						got = append(got, img.Y[i:i+img.Rect.Dx()]...)
					}
					for _, p := range [][]byte{img.Cb, img.Cr} {
						for y := 0; y < (img.Rect.Dy()+1)/2; y++ {
							i := y * img.CStride
							got = append(got, p[i:i+(img.Rect.Dx()+1)/2]...)
						}
					}
				}
			}
			decode := func() {
				frames, err := dec.Decode(unit)
				if err != nil {
					t.Fatalf("access unit %d: %v", count, err)
				}
				appendFrames(frames)
				count++
				unit = nil
			}
			for _, nal := range nals {
				if nal[0]&31 == 9 && len(unit) > 0 {
					decode()
				}
				unit = append(unit, nal)
			}
			if len(unit) > 0 {
				decode()
			}
			appendFrames(dec.Flush())
			if !bytes.Equal(got, want) {
				if len(got) != len(want) {
					t.Fatalf("decoded bytes %d want %d", len(got), len(want))
				}
				count, first, maxDiff := 0, -1, 0
				for i, v := range got {
					if v != want[i] {
						count++
						if first < 0 {
							first = i
						}
						diff := int(v) - int(want[i])
						if diff < 0 {
							diff = -diff
						}
						if diff > maxDiff {
							maxDiff = diff
						}
					}
				}
				t.Fatalf("%d differing samples, first %d: got %d want %d; max difference %d", count, first, got[first], want[first], maxDiff)
			}
		})
	}
}

func BenchmarkH264Decode(b *testing.B) {
	for _, name := range []string{"intra-high-cabac", "intra-360p"} {
		b.Run(name, func(b *testing.B) {
			raw, err := os.ReadFile("testdata/h264/" + name + ".264")
			if err != nil {
				b.Fatal(err)
			}
			nals, err := h264.SplitAnnexB(raw)
			if err != nil {
				b.Fatal(err)
			}
			dec := h264.NewDecoder(h264.Config{})
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for b.Loop() {
				dec.Reset()
				frames, err := dec.Decode(nals)
				if err != nil || len(frames) != 1 {
					b.Fatalf("decode: %v (%d frames)", err, len(frames))
				}
			}
		})
	}
}

func BenchmarkH264DecodeStream(b *testing.B) {
	for _, name := range []string{"motion-baseline", "motion-high-cabac", "motion-b-pyramid"} {
		b.Run(name, func(b *testing.B) {
			raw, err := os.ReadFile("testdata/h264/" + name + ".264")
			if err != nil {
				b.Fatal(err)
			}
			nals, err := h264.SplitAnnexB(raw)
			if err != nil {
				b.Fatal(err)
			}
			var units [][][]byte
			var unit [][]byte
			for _, nal := range nals {
				if nal[0]&31 == 9 && len(unit) > 0 {
					units = append(units, unit)
					unit = nil
				}
				unit = append(unit, nal)
			}
			units = append(units, unit)
			dec := h264.NewDecoder(h264.Config{})
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			frames := 0
			for b.Loop() {
				dec.Reset()
				for _, au := range units {
					out, err := dec.Decode(au)
					if err != nil {
						b.Fatal(err)
					}
					frames += len(out)
				}
				frames += len(dec.Flush())
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(frames), "ns/frame")
		})
	}
}

func TestH264ParameterChangesAndOwnership(t *testing.T) {
	dec := h264.NewDecoder(h264.Config{})
	var first *h264.Frame
	var before []byte
	for _, name := range []string{"intra-baseline", "intra-crop", "intra-high-cabac"} {
		data, err := os.ReadFile("testdata/h264/" + name + ".264")
		if err != nil {
			t.Fatal(err)
		}
		nals, err := h264.SplitAnnexB(data)
		if err != nil {
			t.Fatal(err)
		}
		frames, err := dec.Decode(nals)
		if err != nil || len(frames) != 1 {
			t.Fatalf("%s: %v", name, err)
		}
		if first == nil {
			first = frames[0]
			before = append([]byte(nil), first.Pixels.Y...)
		}
		limited := h264.NewDecoder(h264.Config{MaxPixels: 32 * 32})
		if _, err := limited.Decode(nals); !errors.Is(err, h264.ErrUnsupported) {
			t.Fatalf("pixel limit: %v", err)
		}
	}
	if !bytes.Equal(first.Pixels.Y, before) {
		t.Fatal("retained output frame changed after subsequent decoding")
	}
	dec.Discontinuity()
	if _, err := dec.Decode([][]byte{{0x41, 0x80}}); !errors.Is(err, h264.ErrNeedIDR) {
		t.Fatalf("after discontinuity: %v", err)
	}
}

func TestH264CABACTrailingData(t *testing.T) {
	raw, err := os.ReadFile("testdata/h264/intra-cabac.264")
	if err != nil {
		t.Fatal(err)
	}
	nals, err := h264.SplitAnnexB(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		tail  []byte
		valid bool
	}{
		{"two zero words", []byte{0, 0, 3, 0, 0, 3}, true},
		{"half zero word", []byte{0}, false},
		{"nonzero word", []byte{0, 1}, false},
		{"extra slice data", []byte{0x80, 0x80}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := append([][]byte(nil), nals...)
			for i, n := range changed {
				if n[0]&31 == 5 {
					changed[i] = append(append([]byte(nil), n...), tc.tail...)
				}
			}
			dec := h264.NewDecoder(h264.Config{})
			_, err := dec.Decode(changed)
			if tc.valid && err != nil || !tc.valid && !errors.Is(err, h264.ErrMalformed) {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
