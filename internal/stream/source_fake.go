package stream

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"sync"
)

// FakeSource is a FrameSource that synthesizes JPEG frames in-process. It
// backs unit tests and the --mock daemon on non-macOS hosts: each frame is a
// solid color that cycles per capture, so viewers (and tests) can observe
// frame progression without any simulator.
type FakeSource struct {
	udid  string
	frame func() ([]byte, error) // nil: color-cycling frames

	mu     sync.Mutex
	n      int
	closed bool
}

// NewFakeSource returns a FakeSource labeled with the target udid.
func NewFakeSource(udid string) *FakeSource {
	return &FakeSource{udid: udid}
}

// NewFakeSourceFrames returns a FakeSource whose frames come from the
// given renderer instead of the color cycle — the --mock daemon uses it
// to stream the mock action backend's synthetic UI, so /view and /dash
// show the same screen the actions drive.
func NewFakeSourceFrames(udid string, frame func() ([]byte, error)) *FakeSource {
	return &FakeSource{udid: udid, frame: frame}
}

// FakeSourceFactory is a SourceFactory producing FakeSources.
func FakeSourceFactory(udid string) (FrameSource, error) {
	return NewFakeSource(udid), nil
}

func (f *FakeSource) Next(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, fmt.Errorf("fake source for %s is closed", f.udid)
	}
	f.n++
	if f.frame != nil {
		return f.frame()
	}
	return encodeFakeFrame(f.n)
}

func (f *FakeSource) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// encodeFakeFrame renders a small solid-color JPEG whose hue cycles with n.
func encodeFakeFrame(n int) ([]byte, error) {
	palette := []color.RGBA{
		{R: 0xE5, G: 0x39, B: 0x35, A: 0xFF},
		{R: 0x43, G: 0xA0, B: 0x47, A: 0xFF},
		{R: 0x1E, G: 0x88, B: 0xE5, A: 0xFF},
		{R: 0xFD, G: 0xD8, B: 0x35, A: 0xFF},
	}
	c := palette[n%len(palette)]
	img := image.NewRGBA(image.Rect(0, 0, 96, 96))
	for i := range img.Pix {
		switch i % 4 {
		case 0:
			img.Pix[i] = c.R
		case 1:
			img.Pix[i] = c.G
		case 2:
			img.Pix[i] = c.B
		default:
			img.Pix[i] = c.A
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 60}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
