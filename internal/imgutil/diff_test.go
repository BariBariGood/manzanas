package imgutil

import (
	"image"
	"math"
	"testing"
)

func solidGray(w, h int, v uint8) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = v
	}
	return img
}

// patchGray returns a copy of img with rect set to v.
func patchGray(img *image.Gray, rect image.Rectangle, v uint8) *image.Gray {
	out := image.NewGray(img.Bounds())
	copy(out.Pix, img.Pix)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			out.Pix[out.PixOffset(x, y)] = v
		}
	}
	return out
}

func TestMeanAbsDiff(t *testing.T) {
	base := solidGray(10, 10, 0)
	quarter := image.Rect(0, 0, 5, 5) // 25 of 100 pixels
	tests := []struct {
		name  string
		a, b  *image.Gray
		masks []image.Rectangle
		want  float64
	}{
		{"identical", solidGray(10, 10, 100), solidGray(10, 10, 100), nil, 0},
		{"full range", solidGray(10, 10, 0), solidGray(10, 10, 255), nil, 1},
		{"half step", solidGray(8, 8, 100), solidGray(8, 8, 151), nil, 0.2},
		{"symmetric", solidGray(8, 8, 151), solidGray(8, 8, 100), nil, 0.2},
		{"quarter changed", base, patchGray(base, quarter, 255), nil, 0.25},
		{"masked change ignored", base, patchGray(base, quarter, 255), []image.Rectangle{quarter}, 0},
		{"all masked", solidGray(4, 4, 0), solidGray(4, 4, 255), []image.Rectangle{image.Rect(0, 0, 4, 4)}, 0},
		{"no overlap", solidGray(0, 0, 0), solidGray(4, 4, 255), nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MeanAbsDiff(tt.a, tt.b, tt.masks)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("MeanAbsDiff = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMeanAbsDiffMaskNonZeroOrigin(t *testing.T) {
	// Masks are in the image's own coordinate space, so an image with a
	// non-zero origin (e.g. a SubImage) is masked at its Bounds
	// coordinates, not zero-based ones.
	mk := func(v uint8) *image.Gray {
		img := image.NewGray(image.Rect(10, 20, 20, 30))
		for i := range img.Pix {
			img.Pix[i] = v
		}
		return img
	}
	a, b := mk(0), mk(255)
	if got := MeanAbsDiff(a, b, []image.Rectangle{image.Rect(10, 20, 20, 30)}); got != 0 {
		t.Fatalf("mask at image bounds: MeanAbsDiff = %v, want 0", got)
	}
	if got := MeanAbsDiff(a, b, []image.Rectangle{image.Rect(0, 0, 10, 10)}); got != 1 {
		t.Fatalf("zero-based mask must not apply: MeanAbsDiff = %v, want 1", got)
	}
}

func TestMeanAbsDiffSizeMismatch(t *testing.T) {
	// Compared over the overlapping region only.
	a := solidGray(10, 10, 0)
	b := solidGray(6, 6, 255)
	if got := MeanAbsDiff(a, b, nil); got != 1 {
		t.Fatalf("MeanAbsDiff over overlap = %v, want 1", got)
	}
}
