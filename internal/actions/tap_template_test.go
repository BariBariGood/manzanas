package actions

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/templates"
)

// Synthetic-frame unit tests for the tap_template core: capture, NCC
// match, centre tap, coordinate scaling, and the no-match error shape.

// texturedFrame builds a deterministic non-uniform grayscale-ish RGBA
// frame so any crop of it is a matchable template.
func texturedFrame(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// A deterministic block-noise texture (4x4 blocks so it
			// survives MatchNCC's quarter-res prescan) with no
			// translational self-similarity: every crop matches at
			// exactly one spot.
			n := uint32(x/4)*2654435761 ^ uint32(y/4)*2246822519
			n ^= n >> 13
			n *= 2654435761
			v := uint8(n >> 24)
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = v, v, v, 255
		}
	}
	return img
}

func cropPNGBytes(t *testing.T, img *image.RGBA, r image.Rectangle) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img.SubImage(r)); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func staticCapture(img image.Image, backend string) frameCaptureFunc {
	return func(ctx context.Context) (image.Image, string, error) {
		return img, backend, nil
	}
}

func fixedSpace(scale float64) tapSpaceFunc {
	return func(ctx context.Context, imgW int) (float64, error) { return scale, nil }
}

type tapRecorder struct {
	x, y  float64
	calls int
}

func (r *tapRecorder) tap(ctx context.Context, x, y float64) error {
	r.x, r.y, r.calls = x, y, r.calls+1
	return nil
}

func TestTapTemplateInlineTapsCenter(t *testing.T) {
	frame := texturedFrame(200, 200)
	tmpl := cropPNGBytes(t, frame, image.Rect(60, 80, 90, 100)) // 30x20 at (60, 80)
	rec := &tapRecorder{}
	res, err := tapTemplate(context.Background(), map[string]any{
		"template_base64": base64.StdEncoding.EncodeToString(tmpl),
	}, nil, staticCapture(frame, "mock"), fixedSpace(1), rec.tap)
	if err != nil {
		t.Fatalf("tap_template: %v", err)
	}
	if rec.calls != 1 || rec.x != 75 || rec.y != 90 {
		t.Fatalf("tapped %d time(s) at (%v, %v), want once at (75, 90)", rec.calls, rec.x, rec.y)
	}
	if res["matched"] != true || res["x"] != 75.0 || res["y"] != 90.0 {
		t.Fatalf("result = %v", res)
	}
	if s := res["score"].(float64); s < 0.99 {
		t.Fatalf("score = %v, want ~1", s)
	}
}

func TestTapTemplateScalesCaptureToTapSpace(t *testing.T) {
	frame := texturedFrame(200, 200)
	tmpl := cropPNGBytes(t, frame, image.Rect(60, 80, 90, 100))
	rec := &tapRecorder{}
	// A 2x capture: pixel centre (75, 90) is tap point (37.5, 45) -> (38, 45).
	_, err := tapTemplate(context.Background(), map[string]any{
		"template_base64": base64.StdEncoding.EncodeToString(tmpl),
	}, nil, staticCapture(frame, "mock"), fixedSpace(2), rec.tap)
	if err != nil {
		t.Fatalf("tap_template: %v", err)
	}
	if rec.x != 38 || rec.y != 45 {
		t.Fatalf("tapped (%v, %v), want (38, 45)", rec.x, rec.y)
	}
}

func TestTapTemplateNamedLookupRecordsScore(t *testing.T) {
	frame := texturedFrame(200, 200)
	store, err := templates.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	crop := cropPNGBytes(t, frame, image.Rect(60, 80, 90, 100))
	if _, err := store.Put("_global", "target", crop, templates.Meta{DeviceClass: "mock/200"}); err != nil {
		t.Fatal(err)
	}
	rec := &tapRecorder{}
	res, err := tapTemplate(context.Background(), map[string]any{"name": "target"},
		store, staticCapture(frame, "mock"), fixedSpace(1), rec.tap)
	if err != nil {
		t.Fatalf("tap_template: %v", err)
	}
	if res["name"] != "target" || res["bundle_id"] != "_global" || rec.x != 75 || rec.y != 90 {
		t.Fatalf("result = %v, tap (%v, %v)", res, rec.x, rec.y)
	}
	_, meta, err := store.Get("_global", "target", "mock/200")
	if err != nil {
		t.Fatal(err)
	}
	if meta.MinScoreSeen <= 0 {
		t.Fatalf("min_score_seen = %v, want > 0", meta.MinScoreSeen)
	}
}

func TestTapTemplateNamedWrongClassNotFound(t *testing.T) {
	frame := texturedFrame(200, 200)
	store, err := templates.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	crop := cropPNGBytes(t, frame, image.Rect(60, 80, 90, 100))
	// Saved under a different device class: named lookup must miss.
	if _, err := store.Put("_global", "target", crop, templates.Meta{DeviceClass: "mirror/318"}); err != nil {
		t.Fatal(err)
	}
	_, err = tapTemplate(context.Background(), map[string]any{"name": "target"},
		store, staticCapture(frame, "mock"), fixedSpace(1), (&tapRecorder{}).tap)
	ae, ok := err.(*Error)
	if !ok || ae.Code != "not_found" {
		t.Fatalf("err = %v, want not_found", err)
	}
}

func TestTapTemplateNoMatchCarriesBestScore(t *testing.T) {
	frame := texturedFrame(200, 200)
	// A template that is nowhere in the frame: a reversed gradient.
	other := texturedFrame(40, 40)
	for i := range other.Pix {
		other.Pix[i] = 255 - other.Pix[i]
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, other); err != nil {
		t.Fatal(err)
	}
	rec := &tapRecorder{}
	_, err := tapTemplate(context.Background(), map[string]any{
		"template_base64": base64.StdEncoding.EncodeToString(buf.Bytes()),
		"timeout_ms":      200, "interval_ms": 50,
	}, nil, staticCapture(frame, "mock"), fixedSpace(1), rec.tap)
	if err == nil {
		t.Fatal("want no-match error")
	}
	ae, ok := err.(*Error)
	if !ok || ae.Code != "timeout" {
		t.Fatalf("err = %v, want timeout", err)
	}
	if !strings.Contains(ae.Message, "best") {
		t.Fatalf("message = %q, want best score", ae.Message)
	}
	if rec.calls != 0 {
		t.Fatalf("tapped %d time(s) on a no-match", rec.calls)
	}
}

func TestTapTemplateRectConstrainsSearch(t *testing.T) {
	// Two identical patches; the rect forces the second one.
	frame := image.NewRGBA(image.Rect(0, 0, 200, 200))
	patch := texturedFrame(30, 20)
	for _, at := range []image.Point{{X: 10, Y: 10}, {X: 120, Y: 150}} {
		for y := 0; y < 20; y++ {
			for x := 0; x < 30; x++ {
				i := frame.PixOffset(at.X+x, at.Y+y)
				j := patch.PixOffset(x, y)
				copy(frame.Pix[i:i+4], patch.Pix[j:j+4])
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, patch); err != nil {
		t.Fatal(err)
	}
	rec := &tapRecorder{}
	res, err := tapTemplate(context.Background(), map[string]any{
		"template_base64": base64.StdEncoding.EncodeToString(buf.Bytes()),
		"rect":            map[string]any{"x": 100, "y": 100, "w": 100, "h": 100},
	}, nil, staticCapture(frame, "mock"), fixedSpace(1), rec.tap)
	if err != nil {
		t.Fatalf("tap_template: %v", err)
	}
	if rec.x != 135 || rec.y != 160 {
		t.Fatalf("tapped (%v, %v), want (135, 160) inside the rect", rec.x, rec.y)
	}
	if res["matched"] != true {
		t.Fatalf("result = %v", res)
	}
}

func TestTapTemplateRectExceedingCaptureFails(t *testing.T) {
	frame := texturedFrame(100, 100)
	tmpl := cropPNGBytes(t, frame, image.Rect(10, 10, 40, 30))
	_, err := tapTemplate(context.Background(), map[string]any{
		"template_base64": base64.StdEncoding.EncodeToString(tmpl),
		"rect":            map[string]any{"x": 50, "y": 50, "w": 100, "h": 100},
	}, nil, staticCapture(frame, "mock"), fixedSpace(1), (&tapRecorder{}).tap)
	ae, ok := err.(*Error)
	if !ok || ae.Code != "bad_request" || !strings.Contains(ae.Message, "exceeds") {
		t.Fatalf("err = %v, want bad_request exceeds-capture", err)
	}
}

func TestTapTemplateRejectsNameAndInline(t *testing.T) {
	_, err := tapTemplate(context.Background(), map[string]any{
		"name": "x", "template_base64": "eA==",
	}, nil, staticCapture(texturedFrame(50, 50), "mock"), fixedSpace(1), (&tapRecorder{}).tap)
	ae, ok := err.(*Error)
	if !ok || ae.Code != "bad_request" {
		t.Fatalf("err = %v, want bad_request", err)
	}
}

// --- template resolver rung ---

func TestTemplateSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Sign In":      "sign-in",
		"wifi_toggle":  "wifi_toggle",
		"  Log  In!! ": "log-in",
		"":             "",
	} {
		if got := templateSlug(in); got != want {
			t.Fatalf("templateSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTemplateNameCandidates(t *testing.T) {
	got := templateNameCandidates(predicate{ID: "login_btn", Label: "Sign In"})
	want := []string{"login_btn", "sign-in"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", got, want)
		}
	}
	if c := templateNameCandidates(predicate{Role: "Button"}); c != nil {
		t.Fatalf("role-only predicate candidates = %v, want none", c)
	}
}

// failingRung is an AX-like rung whose source always fails, standing in
// for a dead accessibility bridge.
type failingRung struct{}

func (failingRung) name() string          { return resolverAX }
func (failingRung) budget() time.Duration { return 100 * time.Millisecond }
func (failingRung) resolve(ctx context.Context, udid string, m elementMatcher, refresh bool) (*Node, observation, error) {
	return nil, observation{}, unavailable("ax source dead")
}

// nullDriver satisfies elementDriver for ladder tests that never tap
// through the driver itself.
type nullDriver struct{}

func (nullDriver) observeOnce(ctx context.Context, udid string, refresh bool) (observation, error) {
	return observation{}, unavailable("no source")
}
func (nullDriver) tapXY(ctx context.Context, udid string, x, y float64) error { return nil }
func (nullDriver) typeInto(ctx context.Context, udid, text string, opts typeOpts) (int, error) {
	return 0, unavailable("no source")
}

// The template rung recovers a tap_element whose AX rung fails: a saved
// template keyed by the matcher's label resolves with provenance
// resolver:"template".
func TestResolverLadderTemplateRungRecoversAXFailure(t *testing.T) {
	frame := texturedFrame(200, 200)
	store, err := templates.NewStore(filepath.Join(t.TempDir(), "tpl"))
	if err != nil {
		t.Fatal(err)
	}
	crop := cropPNGBytes(t, frame, image.Rect(60, 80, 90, 100))
	if _, err := store.Put("_global", "sign-in", crop, templates.Meta{DeviceClass: "mock/200"}); err != nil {
		t.Fatal(err)
	}
	lad := resolverLadder{rungs: []resolver{failingRung{}, &templateResolver{
		store:   store,
		capture: func(udid string) frameCaptureFunc { return staticCapture(frame, "mock") },
		space:   func(udid string) tapSpaceFunc { return fixedSpace(1) },
		bundle:  func(udid string) string { return "" },
	}}}
	hit, _, res, err := resolveLadder(context.Background(), nullDriver{}, "UDID",
		map[string]any{"label": "Sign In", "timeout_ms": 2000.0, "interval_ms": 50.0}, "center", lad)
	if err != nil {
		t.Fatalf("resolveLadder: %v", err)
	}
	if res["resolver"] != "template" {
		t.Fatalf("resolver = %v, want template", res["resolver"])
	}
	if hit.Frame == nil || hit.Frame.X != 60 || hit.Frame.Y != 80 || hit.Frame.W != 30 || hit.Frame.H != 20 {
		t.Fatalf("hit frame = %+v, want 30x20 at (60, 80)", hit.Frame)
	}
}

// A predicate that keys no saved template leaves the rung unable to
// serve, so a hard-failing source rung still fails fast with its
// actionable error instead of burning the wait budget.
func TestResolverLadderTemplateRungCannotServeFailsFast(t *testing.T) {
	frame := texturedFrame(200, 200)
	store, err := templates.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lad := resolverLadder{rungs: []resolver{failingRung{}, &templateResolver{
		store:   store,
		capture: func(udid string) frameCaptureFunc { return staticCapture(frame, "mock") },
		space:   func(udid string) tapSpaceFunc { return fixedSpace(1) },
		bundle:  func(udid string) string { return "" },
	}}}
	start := time.Now()
	_, _, _, err = resolveLadder(context.Background(), nullDriver{}, "UDID",
		map[string]any{"label": "Missing", "timeout_ms": 5000.0, "interval_ms": 50.0}, "center", lad)
	ae, ok := err.(*Error)
	if !ok || ae.Code != "unavailable" || !strings.Contains(ae.Message, "ax source dead") {
		t.Fatalf("err = %v, want the AX rung's fast unavailable error", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("fail-fast took %s, want well under the 5s budget", time.Since(start))
	}
}

// With the AX circuit open and no stored template to try, the ladder
// breaks out on the first cycle instead of polling out the wait budget.
func TestResolverLadderCircuitOpenNoTemplateBreaksFast(t *testing.T) {
	frame := texturedFrame(200, 200)
	store, err := templates.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	breaker := newAXBreaker()
	key := breakerKey(context.Background(), "UDID", "")
	for i := 0; i < axBreakerThreshold; i++ {
		breaker.fail(key)
	}
	lad := resolverLadder{
		rungs: []resolver{failingRung{}, &templateResolver{
			store:   store,
			capture: func(udid string) frameCaptureFunc { return staticCapture(frame, "mock") },
			space:   func(udid string) tapSpaceFunc { return fixedSpace(1) },
			bundle:  func(udid string) string { return "" },
		}},
		breaker: breaker,
	}
	start := time.Now()
	_, _, _, err = resolveLadder(context.Background(), nullDriver{}, "UDID",
		map[string]any{"label": "Missing", "timeout_ms": 5000.0, "interval_ms": 50.0}, "center", lad)
	if err == nil {
		t.Fatal("want error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("break took %s, want well under the 5s budget", time.Since(start))
	}
}

// A stored template that never matches the screen is an honest miss:
// the ladder keeps polling and escalates with a timeout carrying the
// template rung's trail.
func TestResolverLadderTemplateRungMisses(t *testing.T) {
	frame := texturedFrame(200, 200)
	store, err := templates.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A uniform crop is degenerate under NCC (score 0): stored, but it
	// can never reach the threshold against the textured frame.
	flat := image.NewGray(image.Rect(0, 0, 30, 20))
	for i := range flat.Pix {
		flat.Pix[i] = 128
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, flat); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("_global", "missing", buf.Bytes(), templates.Meta{DeviceClass: "mock/200"}); err != nil {
		t.Fatal(err)
	}
	lad := resolverLadder{rungs: []resolver{failingRung{}, &templateResolver{
		store:   store,
		capture: func(udid string) frameCaptureFunc { return staticCapture(frame, "mock") },
		space:   func(udid string) tapSpaceFunc { return fixedSpace(1) },
		bundle:  func(udid string) string { return "" },
	}}}
	_, _, _, err = resolveLadder(context.Background(), nullDriver{}, "UDID",
		map[string]any{"label": "Missing", "timeout_ms": 200.0, "interval_ms": 50.0}, "center", lad)
	ae, ok := err.(*Error)
	if !ok || ae.Code != "timeout" || !strings.Contains(ae.Message, "template=no-match") {
		t.Fatalf("err = %v, want timeout with template=no-match trail", err)
	}
}
