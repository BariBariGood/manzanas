package imgutil

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func whitePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func rgbAt(t *testing.T, data []byte, x, y int) (uint32, uint32, uint32) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode annotated png: %v", err)
	}
	r, g, b, _ := img.At(x, y).RGBA()
	return r >> 8, g >> 8, b >> 8
}

func TestAnnotateDrawsBoxAndLabel(t *testing.T) {
	out, err := Annotate(whitePNG(t, 200, 200), []Box{{X: 40, Y: 40, W: 80, H: 60, Label: "F1"}})
	if err != nil {
		t.Fatalf("annotate: %v", err)
	}
	// Top border stroke is red.
	if r, g, b := rgbAt(t, out, 41, 41); r != 220 || g != 30 || b != 30 {
		t.Fatalf("border pixel = rgb(%d,%d,%d), want annotation red", r, g, b)
	}
	// Box interior is untouched.
	if r, g, b := rgbAt(t, out, 80, 70); r != 255 || g != 255 || b != 255 {
		t.Fatalf("interior pixel = rgb(%d,%d,%d), want white", r, g, b)
	}
	// The label tag sits above the box (red background).
	if r, g, b := rgbAt(t, out, 41, 35); r != 220 || g != 30 || b != 30 {
		t.Fatalf("tag pixel = rgb(%d,%d,%d), want annotation red", r, g, b)
	}
}

func TestAnnotateClampsOutOfBoundsBoxes(t *testing.T) {
	out, err := Annotate(whitePNG(t, 100, 100),
		[]Box{{X: 80, Y: -10, W: 200, H: 50, Label: "F2"}, {X: -30, Y: 90, W: 20, H: 40}})
	if err != nil {
		t.Fatalf("annotate with out-of-bounds boxes: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("empty output")
	}
}

func TestAnnotateNoBoxes(t *testing.T) {
	out, err := Annotate(whitePNG(t, 50, 50), nil)
	if err != nil {
		t.Fatalf("annotate: %v", err)
	}
	if r, g, b := rgbAt(t, out, 25, 25); r != 255 || g != 255 || b != 255 {
		t.Fatalf("pixel = rgb(%d,%d,%d), want white", r, g, b)
	}
}

func TestAnnotateRejectsGarbage(t *testing.T) {
	if _, err := Annotate([]byte("not an image"), nil); err == nil {
		t.Fatal("expected decode error")
	}
}
