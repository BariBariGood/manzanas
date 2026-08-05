package imgutil

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTranscodeJPEGDownscale(t *testing.T) {
	src := testPNG(t, 100, 50)
	out, err := Transcode(src, "jpeg", 70, 10)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" {
		t.Fatalf("format = %q, want jpeg", format)
	}
	if cfg.Width != 10 || cfg.Height != 5 {
		t.Fatalf("dims = %dx%d, want 10x5 (aspect preserved)", cfg.Width, cfg.Height)
	}
}

func TestTranscodePNGKeepsSizeWithoutMaxDim(t *testing.T) {
	src := testPNG(t, 40, 40)
	out, err := Transcode(src, "png", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || format != "png" || cfg.Width != 40 || cfg.Height != 40 {
		t.Fatalf("got %s %dx%d (%v), want png 40x40", format, cfg.Width, cfg.Height, err)
	}
}

func TestTranscodeRejectsBadInput(t *testing.T) {
	if _, err := Transcode([]byte("not an image"), "jpeg", 0, 0); err == nil {
		t.Fatal("want decode error")
	}
	if _, err := Transcode(testPNG(t, 4, 4), "gif", 0, 0); err == nil {
		t.Fatal("want unsupported format error")
	}
}

func TestDownscaleNoopWhenSmall(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	if got := Downscale(img, 16); got != img {
		t.Fatal("small image should pass through unchanged")
	}
	if got := Downscale(img, 0); got != img {
		t.Fatal("maxDim 0 should pass through unchanged")
	}
}
