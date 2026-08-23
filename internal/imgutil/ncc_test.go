package imgutil

import (
	"bytes"
	"image"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

func loadPNG(t testing.TB, name string) image.Image {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return img
}

// noisyGray builds a deterministic pseudo-random textured frame so NCC
// has structure to lock onto.
func noisyGray(w, h int, seed int64) *image.Gray {
	rng := rand.New(rand.NewSource(seed))
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.Intn(256))
	}
	return img
}

func cropGray(img *image.Gray, r image.Rectangle) *image.Gray {
	out := image.NewGray(image.Rect(0, 0, r.Dx(), r.Dy()))
	for y := 0; y < r.Dy(); y++ {
		for x := 0; x < r.Dx(); x++ {
			out.Pix[out.PixOffset(x, y)] = img.GrayAt(r.Min.X+x, r.Min.Y+y).Y
		}
	}
	return out
}

func TestMatchNCCSynthetic(t *testing.T) {
	frame := noisyGray(200, 300, 1)
	tests := []struct {
		name   string
		region image.Rectangle
	}{
		{"large template quarter-res path", image.Rect(60, 120, 140, 180)}, // 80x60
		{"medium template half-res path", image.Rect(30, 40, 42, 52)},      // 12x12
		{"tiny template full-res path", image.Rect(150, 250, 156, 256)},    // 6x6
		{"corner origin", image.Rect(0, 0, 40, 40)},
		{"bottom-right edge", image.Rect(160, 260, 200, 300)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl := cropGray(frame, tt.region)
			x, y, score := MatchNCC(frame, tmpl)
			if x != tt.region.Min.X || y != tt.region.Min.Y {
				t.Fatalf("match at (%d,%d), want (%d,%d) (score %.3f)", x, y, tt.region.Min.X, tt.region.Min.Y, score)
			}
			if score < 0.999 {
				t.Fatalf("score = %.4f, want ~1 for exact crop", score)
			}
		})
	}
}

func TestMatchNCCSubImageFrame(t *testing.T) {
	// A SubImage-derived frame has a non-zero origin; the match must be
	// reported in the frame's own coordinate space.
	full := noisyGray(200, 200, 4)
	sub := full.SubImage(image.Rect(50, 60, 180, 190)).(*image.Gray)
	region := image.Rect(90, 100, 130, 140) // inside the sub-image, absolute coords
	tmpl := cropGray(full, region)
	x, y, score := MatchNCC(sub, tmpl)
	if x != region.Min.X || y != region.Min.Y {
		t.Fatalf("match at (%d,%d), want (%d,%d) (score %.3f)", x, y, region.Min.X, region.Min.Y, score)
	}
	if score < 0.999 {
		t.Fatalf("score = %.4f, want ~1 for exact crop", score)
	}
}

func TestMatchNCCDegenerate(t *testing.T) {
	frame := noisyGray(50, 50, 2)
	tests := []struct {
		name  string
		frame *image.Gray
		tmpl  *image.Gray
	}{
		{"template larger than frame", frame, noisyGray(60, 60, 3)},
		{"uniform template", frame, solidGray(20, 20, 128)},
		{"empty template", frame, image.NewGray(image.Rect(0, 0, 0, 0))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x, y, score := MatchNCC(tt.frame, tt.tmpl)
			if x != 0 || y != 0 || score != 0 {
				t.Fatalf("got (%d,%d,%v), want (0,0,0)", x, y, score)
			}
		})
	}
}

// TestMatchNCCFixture matches the cropped "Sign In" button snippet
// against the full mock-backend screenshot it was cropped from. The
// button element lives at (20, 368) in the 390x844 mock screen.
func TestMatchNCCFixture(t *testing.T) {
	frame := GrayDownscale(loadPNG(t, "mock_login.png"), 0)
	tmpl := GrayDownscale(loadPNG(t, "signin_button.png"), 0)
	x, y, score := MatchNCC(frame, tmpl)
	if x != 20 || y != 368 {
		t.Fatalf("match at (%d,%d), want (20,368) (score %.3f)", x, y, score)
	}
	if score < 0.9 {
		t.Fatalf("score = %.4f, want >= 0.9", score)
	}
}

// TestMatchNCCFixtureShifted verifies the match still lands within the
// prescan refinement window after a small brightness perturbation.
func TestMatchNCCFixturePerturbed(t *testing.T) {
	frame := GrayDownscale(loadPNG(t, "mock_login.png"), 0)
	tmpl := GrayDownscale(loadPNG(t, "signin_button.png"), 0)
	// Uniform brightness shift: NCC is invariant to gain/offset.
	for i, p := range frame.Pix {
		v := int(p) + 20
		if v > 255 {
			v = 255
		}
		frame.Pix[i] = uint8(v)
	}
	x, y, score := MatchNCC(frame, tmpl)
	if x != 20 || y != 368 {
		t.Fatalf("match at (%d,%d), want (20,368) (score %.3f)", x, y, score)
	}
	if score < 0.9 {
		t.Fatalf("score = %.4f, want >= 0.9", score)
	}
}

func TestGrayDownscaleFixture(t *testing.T) {
	// Mirror-frame width (~318px) downscale of the sim-size screenshot.
	g := GrayDownscale(loadPNG(t, "mock_login.png"), 688) // 844*318/844 -> longer edge
	if g.Bounds().Dy() != 688 {
		t.Fatalf("height = %d, want 688", g.Bounds().Dy())
	}
	if g.Bounds().Dx() != 390*688/844 {
		t.Fatalf("width = %d, want %d", g.Bounds().Dx(), 390*688/844)
	}
}

// Benchmarks document ms/frame for NCC at the two real capture sizes:
// mirror frames (~318px wide) and simulator screenshots (390x844).

func benchmarkNCC(b *testing.B, frame *image.Gray, tmpl *image.Gray) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		MatchNCC(frame, tmpl)
	}
}

func BenchmarkMatchNCCSimSize(b *testing.B) {
	// 390x844 sim screenshot, 80x36 template over the "Sign In" label
	// (the button center) so the template has real structure.
	frame := GrayDownscale(loadPNG(b, "mock_login.png"), 0)
	tmpl := cropGray(frame, image.Rect(155, 375, 235, 411))
	benchmarkNCC(b, frame, tmpl)
}

func BenchmarkMatchNCCMirrorSize(b *testing.B) {
	// Mirror frames are ~318px wide; downscale the sim frame to match.
	frame := GrayDownscale(loadPNG(b, "mock_login.png"), 318*844/390)
	// Scale the "Sign In" label region accordingly (~65x29).
	sx := float64(frame.Bounds().Dx()) / 390
	tmpl := cropGray(frame, image.Rect(int(155*sx), int(375*sx), int(235*sx), int(411*sx)))
	benchmarkNCC(b, frame, tmpl)
}

func BenchmarkMeanAbsDiffSimSize(b *testing.B) {
	a := GrayDownscale(loadPNG(b, "mock_login.png"), 0)
	c := GrayDownscale(loadPNG(b, "mock_login_after_tap.png"), 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		MeanAbsDiff(a, c, nil)
	}
}

func BenchmarkGrayDownscaleSimSize(b *testing.B) {
	img := loadPNG(b, "mock_login.png")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		GrayDownscale(img, 318)
	}
}

func BenchmarkDHashSimSize(b *testing.B) {
	img := loadPNG(b, "mock_login.png")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		DHash(img, nil)
	}
}
