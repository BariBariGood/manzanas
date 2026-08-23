package imgutil

import (
	"image"
	"image/color"
	"testing"
)

func solidRGBA(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func TestGrayDownscale(t *testing.T) {
	tests := []struct {
		name         string
		img          image.Image
		maxDim       int
		wantW, wantH int
		wantPix      uint8 // expected uniform value; checked when img is solid
	}{
		{"no scale", solidRGBA(10, 20, color.RGBA{255, 255, 255, 255}), 0, 10, 20, 255},
		{"already small", solidRGBA(10, 20, color.RGBA{0, 0, 0, 255}), 40, 10, 20, 0},
		{"halve tall", solidRGBA(10, 20, color.RGBA{128, 128, 128, 255}), 10, 5, 10, 128},
		{"halve wide", solidRGBA(20, 10, color.RGBA{128, 128, 128, 255}), 10, 10, 5, 128},
		{"clamp to 1", solidRGBA(100, 2, color.RGBA{50, 50, 50, 255}), 10, 10, 1, 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := GrayDownscale(tt.img, tt.maxDim)
			if g.Bounds().Dx() != tt.wantW || g.Bounds().Dy() != tt.wantH {
				t.Fatalf("size = %dx%d, want %dx%d", g.Bounds().Dx(), g.Bounds().Dy(), tt.wantW, tt.wantH)
			}
			for i, p := range g.Pix {
				if p != tt.wantPix {
					t.Fatalf("pix[%d] = %d, want %d", i, p, tt.wantPix)
				}
			}
		})
	}
}

func TestGrayDownscaleLuma(t *testing.T) {
	// Pure red/green/blue map to their Rec. 601 luma weights.
	tests := []struct {
		c    color.RGBA
		want uint8
		tol  int
	}{
		{color.RGBA{255, 0, 0, 255}, 76, 1},
		{color.RGBA{0, 255, 0, 255}, 150, 1},
		{color.RGBA{0, 0, 255, 255}, 29, 1},
	}
	for _, tt := range tests {
		g := GrayDownscale(solidRGBA(4, 4, tt.c), 0)
		got := int(g.Pix[0])
		if got < int(tt.want)-tt.tol || got > int(tt.want)+tt.tol {
			t.Errorf("luma(%v) = %d, want %d±%d", tt.c, got, tt.want, tt.tol)
		}
	}
}

func TestGrayDownscaleZeroDimension(t *testing.T) {
	for _, r := range []image.Rectangle{
		image.Rect(0, 0, 0, 10),
		image.Rect(0, 0, 10, 0),
		image.Rect(0, 0, 0, 0),
	} {
		g := GrayDownscale(image.NewRGBA(r), 5)
		if g.Bounds().Dx() != 0 || g.Bounds().Dy() != 0 {
			t.Fatalf("GrayDownscale(%v) bounds = %v, want empty", r, g.Bounds())
		}
	}
}

func TestGrayDownscaleNonZeroOrigin(t *testing.T) {
	src := image.NewRGBA(image.Rect(5, 7, 15, 17))
	for y := 7; y < 17; y++ {
		for x := 5; x < 15; x++ {
			src.SetRGBA(x, y, color.RGBA{200, 200, 200, 255})
		}
	}
	g := GrayDownscale(src, 5)
	if g.Bounds() != image.Rect(0, 0, 5, 5) {
		t.Fatalf("bounds = %v, want (0,0)-(5,5)", g.Bounds())
	}
	for i, p := range g.Pix {
		if p != 200 {
			t.Fatalf("pix[%d] = %d, want 200", i, p)
		}
	}
}
