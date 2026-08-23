package actions

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
)

func TestNormalizeRun(t *testing.T) {
	cases := []struct {
		in     string
		minLen int
		want   string
	}{
		{"  Wi-Fi  ", 2, "wi-fi"},
		{"GENERAL", 2, "general"},
		{"l", 2, ""},
		{"", 2, ""},
		{"   ", 2, ""},
		{"ok", 2, "ok"},
		{"ok", 3, ""},
		{"héllo", 2, "héllo"},
	}
	for _, c := range cases {
		if got := normalizeRun(c.in, c.minLen); got != c.want {
			t.Errorf("normalizeRun(%q, %d) = %q, want %q", c.in, c.minLen, got, c.want)
		}
	}
}

func TestTextSetNormalizationAndFields(t *testing.T) {
	nodes := []*Node{
		{Role: "Button", Label: "  Sign In  ", Frame: &Frame{X: 10, Y: 100, W: 80, H: 20}},
		{Role: "TextField", Value: "Search", Placeholder: "Type here", Frame: &Frame{X: 10, Y: 150, W: 200, H: 30}},
		{Role: "Text", Label: "|", Frame: &Frame{X: 10, Y: 200, W: 4, H: 10}}, // too short
		{Role: "Group", Children: []*Node{
			{Role: "Text", Label: "Nested Label", Frame: &Frame{X: 10, Y: 300, W: 100, H: 20}},
		}},
		nil,
	}
	got := textSet(nodes, nil)
	want := []string{"sign in", "search", "type here", "nested label"}
	if len(got) != len(want) {
		t.Fatalf("set = %v, want %d entries", got, len(want))
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("missing %q in %v", w, got)
		}
	}
}

func TestJaccardEdgeCases(t *testing.T) {
	set := func(ss ...string) map[string]struct{} {
		m := map[string]struct{}{}
		for _, s := range ss {
			m[s] = struct{}{}
		}
		return m
	}
	cases := []struct {
		name string
		a, b map[string]struct{}
		want float64
	}{
		{"both empty", set(), set(), 1.0},
		{"one empty", set("a"), set(), 0.0},
		{"identical", set("a", "b"), set("a", "b"), 1.0},
		{"disjoint", set("a", "b"), set("c", "d"), 0.0},
		{"half overlap", set("a", "b"), set("b", "c"), 1.0 / 3.0},
		{"nil sets", nil, nil, 1.0},
	}
	for _, c := range cases {
		if got := jaccard(c.a, c.b); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: jaccard = %v, want %v", c.name, got, c.want)
		}
		if got := jaccard(c.b, c.a); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s (swapped): jaccard = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestVolatileBandGeometry(t *testing.T) {
	// Fraction-of-height bands must scale with the viewport: the same
	// clock box is masked at every window size, and band edges follow H.
	sizes := []struct {
		name string
		w, h float64
	}{
		{"mirror-small", 410, 890},
		{"mirror-large", 820, 1780},
		{"sim-points", 393, 852},
	}
	for _, s := range sizes {
		vp := &Frame{W: s.w, H: s.h}
		topEdge := s.h * defaultTopBandFrac
		botEdge := s.h * (1 - defaultBottomBandFrac)
		cases := []struct {
			name string
			box  Frame
			want bool
		}{
			{"clock in top band", Frame{X: 10, Y: 0, W: 40, H: topEdge * 0.5}, true},
			{"just below top band", Frame{X: 10, Y: topEdge + 1, W: 40, H: 20}, false},
			{"mid screen", Frame{X: 10, Y: s.h / 2, W: 40, H: 20}, false},
			{"home indicator", Frame{X: 10, Y: botEdge + 1, W: 40, H: (s.h - botEdge - 1) * 0.9}, true},
			{"just above bottom band", Frame{X: 10, Y: botEdge - 30, W: 40, H: 20}, false},
		}
		for _, c := range cases {
			b := c.box
			got := inVolatileBand(&b, vp, defaultTopBandFrac, defaultBottomBandFrac)
			if got != c.want {
				t.Errorf("%s/%s: inVolatileBand = %v, want %v", s.name, c.name, got, c.want)
			}
		}
	}

	// Viewport with a non-zero origin: bands are relative to viewport.Y.
	vp := &Frame{Y: 100, W: 400, H: 800}
	clock := &Frame{X: 10, Y: 105, W: 40, H: 18}
	if !inVolatileBand(clock, vp, defaultTopBandFrac, defaultBottomBandFrac) {
		t.Error("clock at offset-viewport top not masked")
	}
	mid := &Frame{X: 10, Y: 500, W: 40, H: 18}
	if inVolatileBand(mid, vp, defaultTopBandFrac, defaultBottomBandFrac) {
		t.Error("mid box wrongly masked in offset viewport")
	}

	// nil / zero-height viewport disables masking.
	if inVolatileBand(clock, nil, defaultTopBandFrac, defaultBottomBandFrac) {
		t.Error("nil viewport must disable masking")
	}
	if inVolatileBand(clock, &Frame{W: 400}, defaultTopBandFrac, defaultBottomBandFrac) {
		t.Error("zero-height viewport must disable masking")
	}
	// Negative fractions disable the corresponding band.
	if inVolatileBand(clock, &Frame{Y: 100, W: 400, H: 800}, -1, -1) {
		t.Error("negative fractions must disable both bands")
	}
}

func TestTextSetMasksVolatileBands(t *testing.T) {
	vp := &Frame{W: 400, H: 800}
	nodes := []*Node{
		{Role: "Text", Label: "9:41", Frame: &Frame{X: 10, Y: 5, W: 40, H: 18}}, // clock, top band
		{Role: "Text", Label: "Content", Frame: &Frame{X: 10, Y: 400, W: 100, H: 20}},
		{Role: "Text", Label: "Home Bar", Frame: &Frame{X: 150, Y: 790, W: 100, H: 8}}, // bottom band
		{Role: "Text", Label: "No Frame"},                                              // frameless nodes are kept
	}
	got := textSet(nodes, vp)
	if _, ok := got["9:41"]; ok {
		t.Errorf("clock not masked: %v", got)
	}
	if _, ok := got["home bar"]; ok {
		t.Errorf("home indicator strip not masked: %v", got)
	}
	if _, ok := got["content"]; !ok {
		t.Errorf("content missing: %v", got)
	}
	if _, ok := got["no frame"]; !ok {
		t.Errorf("frameless node dropped: %v", got)
	}
}

func TestOCRTextSetConfidenceFloor(t *testing.T) {
	boxes := []mirror.OCRBox{
		{Text: "Solid", Confidence: 0.95, X: 10, Y: 400, W: 80, H: 20},
		{Text: "Noise", Confidence: 0.2, X: 10, Y: 450, W: 80, H: 20},
	}
	got := ocrTextSet(boxes, &Frame{W: 400, H: 800}, textSetOpts{})
	if _, ok := got["solid"]; !ok {
		t.Errorf("high-confidence box missing: %v", got)
	}
	if _, ok := got["noise"]; ok {
		t.Errorf("low-confidence box kept: %v", got)
	}
}

// TestFixtureScrollJaccardSeparation drives the utilities with recorded-style
// OCR box lists (before / real-scroll after / bounce-back after) and asserts
// the Jaccard separation the phone-harness tuning relies on: real progress
// < 0.45, bounce > 0.7.
func TestFixtureScrollJaccardSeparation(t *testing.T) {
	raw, err := os.ReadFile("testdata/textset-ocr-scroll.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		ImgW          float64         `json:"img_w"`
		ImgH          float64         `json:"img_h"`
		Before        []mirror.OCRBox `json:"before"`
		AfterProgress []mirror.OCRBox `json:"after_progress"`
		AfterBounce   []mirror.OCRBox `json:"after_bounce"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	vp := &Frame{W: fx.ImgW, H: fx.ImgH}
	before := ocrTextSet(fx.Before, vp, textSetOpts{})
	progress := ocrTextSet(fx.AfterProgress, vp, textSetOpts{})
	bounce := ocrTextSet(fx.AfterBounce, vp, textSetOpts{})

	// The ticking clock ("9:41" → "9:42") must not appear in any set.
	for name, s := range map[string]map[string]struct{}{"before": before, "progress": progress, "bounce": bounce} {
		for _, clk := range []string{"9:41", "9:42"} {
			if _, ok := s[clk]; ok {
				t.Errorf("%s: clock %q not masked", name, clk)
			}
		}
	}

	if j := jaccard(before, progress); j >= 0.45 {
		t.Errorf("real progress jaccard = %v, want < 0.45", j)
	}
	if j := jaccard(before, bounce); j <= 0.7 {
		t.Errorf("bounce jaccard = %v, want > 0.7", j)
	}
}
