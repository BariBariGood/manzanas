package imgutil_test

import (
	"bytes"
	"image"
	"image/png"
	"testing"

	"github.com/BariBariGood/manzanas/internal/actions/mockapp"
	"github.com/BariBariGood/manzanas/internal/imgutil"
)

func renderGray(t *testing.T, app *mockapp.App, maxDim int) *image.Gray {
	t.Helper()
	data, err := app.RenderPNG()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return imgutil.GrayDownscale(img, maxDim)
}

// TestMockBackendDiffDemo is the end-to-end demo for the pixel
// primitives: two frames rendered by the real mock backend are
// identical before a UI mutation (MeanAbsDiff == 0, same DHash) and
// measurably different after tapping Sign In makes the status label
// appear (MeanAbsDiff above a stability threshold, DHash unchanged only
// when the changed band is masked).
func TestMockBackendDiffDemo(t *testing.T) {
	const stableThreshold = 0.002 // wait_for_stable-style quiet threshold

	app := mockapp.NewApp()
	before := renderGray(t, app, 0)
	same := renderGray(t, app, 0)
	if d := imgutil.MeanAbsDiff(before, same, nil); d != 0 {
		t.Fatalf("identical frames MeanAbsDiff = %v, want 0", d)
	}
	if imgutil.HashDistance(imgutil.DHash(before, nil), imgutil.DHash(same, nil)) != 0 {
		t.Fatal("identical frames should share a DHash")
	}

	// Mutate the UI: tapping Sign In adds the status label at y=438.
	app.Tap(100, 390)
	after := renderGray(t, app, 0)
	d := imgutil.MeanAbsDiff(before, after, nil)
	if d <= stableThreshold {
		t.Fatalf("post-mutation MeanAbsDiff = %v, want > %v", d, stableThreshold)
	}

	// Masking the status-label band hides the mutation again — the
	// masked-diff behavior wait_for_stable relies on for volatile
	// regions.
	mask := []image.Rectangle{image.Rect(0, 430, 390, 480)}
	if md := imgutil.MeanAbsDiff(before, after, mask); md != 0 {
		t.Fatalf("masked MeanAbsDiff = %v, want 0", md)
	}
}

// TestMockBackendTemplateDemo locates the mock app's Sign In button in
// a live-rendered frame by NCC template match — the tap_template
// primitive path.
func TestMockBackendTemplateDemo(t *testing.T) {
	app := mockapp.NewApp()
	frame := renderGray(t, app, 0)

	// Crop the Sign In button region (element sign-in: 20,368 350x50).
	tmpl := image.NewGray(image.Rect(0, 0, 350, 50))
	for y := 0; y < 50; y++ {
		for x := 0; x < 350; x++ {
			tmpl.Pix[tmpl.PixOffset(x, y)] = frame.GrayAt(20+x, 368+y).Y
		}
	}

	// Mutate unrelated state (toggle Wi-Fi) and re-render: the button
	// must still be found at its location with a high score.
	app.Tap(40, 320)
	moved := renderGray(t, app, 0)
	x, y, score := imgutil.MatchNCC(moved, tmpl)
	if x != 20 || y != 368 {
		t.Fatalf("match at (%d,%d), want (20,368) (score %.3f)", x, y, score)
	}
	if score < 0.9 {
		t.Fatalf("score = %.4f, want >= 0.9", score)
	}
}
