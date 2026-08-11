package mockapp

import (
	"bytes"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"testing"
)

func decode(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestRenderPNGMatchesScreenSize(t *testing.T) {
	a := NewApp()
	png, err := a.RenderPNG()
	if err != nil {
		t.Fatal(err)
	}
	img := decode(t, png)
	if img.Bounds().Dx() != ScreenW || img.Bounds().Dy() != ScreenH {
		t.Fatalf("render size = %v, want %dx%d", img.Bounds(), ScreenW, ScreenH)
	}
}

func TestRenderReflectsState(t *testing.T) {
	a := NewApp()
	before, err := a.RenderPNG()
	if err != nil {
		t.Fatal(err)
	}
	a.Tap(100, 180) // focus username: keyboard appears
	a.Type("ivan")
	after, err := a.RenderPNG()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("screenshot should change when the tree changes")
	}
	// Deterministic: the same state renders the same pixels.
	again, err := a.RenderPNG()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, again) {
		t.Fatal("rendering must be deterministic for an unchanged state")
	}
}

func TestRenderJPEGDecodes(t *testing.T) {
	a := NewApp()
	jpg, err := a.RenderJPEG()
	if err != nil {
		t.Fatal(err)
	}
	img := decode(t, jpg)
	if img.Bounds().Dx() != ScreenW {
		t.Fatalf("jpeg width = %d, want %d", img.Bounds().Dx(), ScreenW)
	}
}
