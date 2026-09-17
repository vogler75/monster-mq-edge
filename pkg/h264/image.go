package h264

import (
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math"
)

// RGB converts the cropped picture to full-range RGB, accounting for VUI range
// and colour matrix. Unspecified matrix coefficients use BT.601. Decoding to
// Pixels remains available even for a colour matrix RGB does not implement.
func (f *Frame) RGB() (*image.RGBA, error) {
	if f == nil || f.Pixels == nil {
		return nil, fmt.Errorf("%w: no decoded picture", ErrMalformed)
	}
	kr, kb := 0.299, 0.114
	switch f.MatrixCoefficients {
	case 1:
		kr, kb = 0.2126, 0.0722
	case 2, 5, 6:
	case 9:
		kr, kb = 0.2627, 0.0593
	default:
		return nil, fmt.Errorf("%w: colour matrix %d", ErrUnsupported, f.MatrixCoefficients)
	}
	ys, cs, yo := 1.0, 1.0, 0
	if !f.FullRange {
		ys, cs, yo = 255.0/219, 255.0/224, 16
	}
	fixed := func(v float64) int { return int(math.Round(v * 65536)) }
	yc, rc, bc := fixed(ys), fixed(cs*2*(1-kr)), fixed(cs*2*(1-kb))
	gcB, gcR := fixed(cs*2*kb*(1-kb)/(1-kr-kb)), fixed(cs*2*kr*(1-kr)/(1-kr-kb))
	src := f.Pixels
	r := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			yi, ci := src.YOffset(x+r.Min.X, y+r.Min.Y), src.COffset(x+r.Min.X, y+r.Min.Y)
			l, cb, cr := yc*(int(src.Y[yi])-yo), int(src.Cb[ci])-128, int(src.Cr[ci])-128
			i := y*out.Stride + x*4
			out.Pix[i] = byte(clip((l+rc*cr+32768)>>16, 0, 255))
			out.Pix[i+1] = byte(clip((l-gcB*cb-gcR*cr+32768)>>16, 0, 255))
			out.Pix[i+2] = byte(clip((l+bc*cb+32768)>>16, 0, 255))
			out.Pix[i+3] = 255
		}
	}
	return out, nil
}

// WriteJPEG encodes a decoded picture with the standard library JPEG encoder.
func (f *Frame) WriteJPEG(w io.Writer, quality int) error {
	img, err := f.RGB()
	if err != nil {
		return err
	}
	return jpeg.Encode(w, img, &jpeg.Options{Quality: quality})
}
