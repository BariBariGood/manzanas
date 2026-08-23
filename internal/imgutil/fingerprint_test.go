package imgutil

import (
	"image"
	"image/color"
	"testing"
)

// gradientRGBA has a left-to-right brightness ramp: every dhash bit is
// deterministic (each cell darker than its right neighbor).
func gradientRGBA(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(x * 255 / (w - 1))
			img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

// checkerRGBA alternates bright/dark blocks at the dhash cell pitch, so
// roughly half the neighbor comparisons flip relative to a ramp.
func checkerRGBA(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(0)
			if (x*9/w+y*8/h)%2 == 0 {
				v = 255
			}
			img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

func TestDHash(t *testing.T) {
	tests := []struct {
		name string
		a, b image.Image
		// wantDist bounds on HashDistance(DHash(a), DHash(b)).
		minDist, maxDist int
	}{
		{"identical images", gradientRGBA(90, 80), gradientRGBA(90, 80), 0, 0},
		{"scale invariant", gradientRGBA(90, 80), gradientRGBA(180, 160), 0, 0},
		{"checkerboard vs gradient", checkerRGBA(90, 80), gradientRGBA(90, 80), 20, 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := HashDistance(DHash(tt.a, nil), DHash(tt.b, nil))
			if d < tt.minDist || d > tt.maxDist {
				t.Fatalf("HashDistance = %d, want in [%d,%d]", d, tt.minDist, tt.maxDist)
			}
		})
	}
}

func TestDHashGradientBits(t *testing.T) {
	// A rising ramp means every cell is darker than its right neighbor:
	// all 64 bits must be 0. The inverted ramp sets all 64.
	rising := DHash(gradientRGBA(90, 80), nil)
	if rising != 0 {
		t.Fatalf("rising ramp hash = %#x, want 0", rising)
	}
	falling := image.NewRGBA(image.Rect(0, 0, 90, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 90; x++ {
			v := uint8(255 - x*255/89)
			falling.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	if h := DHash(falling, nil); h != ^uint64(0) {
		t.Fatalf("falling ramp hash = %#x, want all ones", h)
	}
}

func TestDHashMasks(t *testing.T) {
	base := gradientRGBA(90, 80)
	// Corrupt a band (e.g. a status-bar clock) and confirm masking
	// restores the original fingerprint.
	noisy := gradientRGBA(90, 80)
	for y := 0; y < 10; y++ {
		for x := 0; x < 90; x++ {
			noisy.SetRGBA(x, y, color.RGBA{uint8(x * 7 % 255), 0, 255, 255})
		}
	}
	if DHash(base, nil) == DHash(noisy, nil) {
		t.Fatal("unmasked corruption should change the hash")
	}
	mask := []image.Rectangle{image.Rect(0, 0, 90, 10)}
	if got, want := DHash(noisy, mask), DHash(base, mask); got != want {
		t.Fatalf("masked hashes differ: %#x vs %#x", got, want)
	}
}

func TestDHashEmptyImage(t *testing.T) {
	if h := DHash(image.NewRGBA(image.Rect(0, 0, 0, 0)), nil); h != 0 {
		t.Fatalf("empty image hash = %#x, want 0", h)
	}
}

func TestHashDistance(t *testing.T) {
	tests := []struct {
		a, b uint64
		want int
	}{
		{0, 0, 0},
		{0, 1, 1},
		{0, ^uint64(0), 64},
		{0xF0F0, 0x0F0F, 16},
	}
	for _, tt := range tests {
		if got := HashDistance(tt.a, tt.b); got != tt.want {
			t.Errorf("HashDistance(%#x, %#x) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
