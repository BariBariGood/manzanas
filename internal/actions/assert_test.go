package actions

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/templates"
)

// loginTree is a minimal describe-ui payload with a couple of text runs.
const loginTree = `[{"role":"Application","AXLabel":"App",
  "frame":{"x":0,"y":0,"width":390,"height":844},
  "children":[
    {"role":"StaticText","AXLabel":"Mock Login","frame":{"x":20,"y":80,"width":350,"height":40},"enabled":true},
    {"role":"Button","AXLabel":"Sign In","frame":{"x":20,"y":368,"width":350,"height":50},"enabled":true}
  ]}]`

func TestAssertTextFound(t *testing.T) {
	f := newFakeRunner()
	f.stdout["describe-ui"] = loginTree
	b := testBackend(f)
	res, err := dispatch(t, b, "assert_text", map[string]any{"text": "mock login"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["found"] != true || res.Result["matched"] != "Mock Login" {
		t.Fatalf("result = %v", res.Result)
	}
}

func TestAssertTextExactMissesSubstring(t *testing.T) {
	f := newFakeRunner()
	f.stdout["describe-ui"] = loginTree
	b := testBackend(f)
	_, err := dispatch(t, b, "assert_text", map[string]any{
		"text": "Mock", "fuzzy": false, "timeout_ms": 200, "interval_ms": 50,
	})
	if code := actionCode(t, err); code != "assertion_failed" {
		t.Fatalf("code = %q, want assertion_failed", code)
	}
}

func TestAssertTextAbsentFailsAfterRetries(t *testing.T) {
	f := newFakeRunner()
	f.stdout["describe-ui"] = loginTree
	b := testBackend(f)
	_, err := dispatch(t, b, "assert_text", map[string]any{
		"text": "No Such Text", "timeout_ms": 300, "interval_ms": 50,
	})
	if code := actionCode(t, err); code != "assertion_failed" {
		t.Fatalf("code = %q, want assertion_failed", code)
	}
	// The failure must come after retries, not a single look.
	polls := 0
	for _, argv := range f.argvs() {
		if strings.Contains(argv, "describe-ui") {
			polls++
		}
	}
	if polls < 2 {
		t.Fatalf("polls = %d, want >= 2 (retry after settle)", polls)
	}
}

func TestAssertTextRequiresText(t *testing.T) {
	b := testBackend(newFakeRunner())
	_, err := dispatch(t, b, "assert_text", map[string]any{})
	if code := actionCode(t, err); code != "bad_request" {
		t.Fatalf("code = %q, want bad_request", code)
	}
}

func TestAssertNoText(t *testing.T) {
	f := newFakeRunner()
	f.stdout["describe-ui"] = loginTree
	b := testBackend(f)
	if _, err := dispatch(t, b, "assert_no_text", map[string]any{"text": "Welcome"}); err != nil {
		t.Fatal(err)
	}
	_, err := dispatch(t, b, "assert_no_text", map[string]any{
		"text": "Sign In", "timeout_ms": 200, "interval_ms": 50,
	})
	if code := actionCode(t, err); code != "assertion_failed" {
		t.Fatalf("code = %q, want assertion_failed", code)
	}
}

// assertFrame builds a deterministic non-uniform test frame.
func assertFrame(t *testing.T) *image.RGBA {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 120, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 120; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 2), uint8(y), uint8(x + y), 255})
		}
	}
	// A distinctive block for the template to lock onto.
	for y := 60; y < 90; y++ {
		for x := 40; x < 80; x++ {
			img.Set(x, y, color.RGBA{255, 255, 255, 255})
		}
	}
	return img
}

func pngB64(t *testing.T, img image.Image) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestAssertTemplate(t *testing.T) {
	frame := assertFrame(t)
	var buf bytes.Buffer
	if err := png.Encode(&buf, frame); err != nil {
		t.Fatal(err)
	}
	f := newFakeRunner()
	f.writeFile["screenshot"] = buf.String()
	b := testBackend(f)

	tmpl := frame.SubImage(image.Rect(36, 56, 84, 94))
	res, err := dispatch(t, b, "assert_template", map[string]any{"template_base64": pngB64(t, tmpl)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["found"] != true {
		t.Fatalf("result = %v", res.Result)
	}
	if x, y := res.Result["x"].(int), res.Result["y"].(int); x != 36 || y != 56 {
		t.Fatalf("matched at (%d, %d), want (36, 56)", x, y)
	}
	if score := res.Result["score"].(float64); score < 0.99 {
		t.Fatalf("score = %v, want >= 0.99", score)
	}
}

func TestAssertTemplateNoMatch(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, assertFrame(t)); err != nil {
		t.Fatal(err)
	}
	f := newFakeRunner()
	f.writeFile["screenshot"] = buf.String()
	b := testBackend(f)

	// A template the frame does not contain: an inverted gradient.
	alien := image.NewRGBA(image.Rect(0, 0, 30, 30))
	for y := 0; y < 30; y++ {
		for x := 0; x < 30; x++ {
			if (x/5+y/5)%2 == 0 {
				alien.Set(x, y, color.RGBA{255, 0, 0, 255})
			}
		}
	}
	_, err := dispatch(t, b, "assert_template", map[string]any{
		"template_base64": pngB64(t, alien), "timeout_ms": 200, "interval_ms": 50,
	})
	if code := actionCode(t, err); code != "assertion_failed" {
		t.Fatalf("code = %q, want assertion_failed", code)
	}
}

func TestAssertTemplateNameWithoutStore(t *testing.T) {
	b := testBackend(newFakeRunner())
	_, err := dispatch(t, b, "assert_template", map[string]any{"name": "login-button"})
	if code := actionCode(t, err); code != "unavailable" {
		t.Fatalf("code = %q, want unavailable", code)
	}
}

func TestAssertTemplateNamed(t *testing.T) {
	frame := assertFrame(t)
	var buf bytes.Buffer
	if err := png.Encode(&buf, frame); err != nil {
		t.Fatal(err)
	}
	f := newFakeRunner()
	f.writeFile["screenshot"] = buf.String()
	b := testBackend(f)
	store, err := templates.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b.SetTemplateStore(store)

	// Store the distinctive block under the frame's device class
	// ("axe/120" — the fake screenshot path reports backend "axe").
	var tmplBuf bytes.Buffer
	if err := png.Encode(&tmplBuf, frame.SubImage(image.Rect(36, 56, 84, 94))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("_global", "block", tmplBuf.Bytes(), templates.Meta{
		DeviceClass: "axe/120", WindowPx: [2]int{120, 200},
		CropRect: templates.Rect{X: 36, Y: 56, W: 48, H: 38},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := dispatch(t, b, "assert_template", map[string]any{"name": "block"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["found"] != true || res.Result["name"] != "block" {
		t.Fatalf("result = %v", res.Result)
	}

	// A name the store lacks (for this class) is not_found.
	_, err = dispatch(t, b, "assert_template", map[string]any{"name": "missing"})
	if code := actionCode(t, err); code != "not_found" {
		t.Fatalf("code = %q, want not_found", code)
	}
}

func TestAssertTemplateRequiresPayload(t *testing.T) {
	b := testBackend(newFakeRunner())
	for _, p := range []map[string]any{
		{},
		{"template_base64": "not-base64!"},
		{"template_base64": base64.StdEncoding.EncodeToString([]byte("not an image"))},
	} {
		_, err := dispatch(t, b, "assert_template", p)
		if code := actionCode(t, err); code != "bad_request" {
			t.Fatalf("payload %v: code = %q, want bad_request", p, code)
		}
	}
}

// --- mirror (OCR) assert path ---

func TestMirrorAssertText(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Sign In", "confidence": 0.93, "x": 60, "y": 700, "w": 200, "h": 40},
		{"text": "garbled", "confidence": 0.2, "x": 0, "y": 0, "w": 10, "h": 10},
	}
	res, err := deviceDispatch(t, b, "assert_text", map[string]any{"text": "sign in"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["found"] != true || res.Result["backend"] != "mirror" || res.Result["fidelity"] != "ocr" {
		t.Fatalf("result = %v", res.Result)
	}
}

func TestMirrorAssertTextMinConfidence(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Sign In", "confidence": 0.3, "x": 60, "y": 700, "w": 200, "h": 40},
	}
	_, err := deviceDispatch(t, b, "assert_text", map[string]any{
		"text": "Sign In", "min_confidence": 0.5, "timeout_ms": 200, "interval_ms": 50,
	})
	if code := actionCode(t, err); code != "assertion_failed" {
		t.Fatalf("code = %q, want assertion_failed", code)
	}
	// Without the floor the same box matches.
	if _, err := deviceDispatch(t, b, "assert_text", map[string]any{"text": "Sign In"}); err != nil {
		t.Fatal(err)
	}
}

func TestMirrorAssertNoText(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Welcome back", "confidence": 0.9, "x": 60, "y": 100, "w": 200, "h": 40},
	}
	if _, err := deviceDispatch(t, b, "assert_no_text", map[string]any{"text": "Sign Out"}); err != nil {
		t.Fatal(err)
	}
	_, err := deviceDispatch(t, b, "assert_no_text", map[string]any{
		"text": "welcome", "timeout_ms": 200, "interval_ms": 50,
	})
	if code := actionCode(t, err); code != "assertion_failed" {
		t.Fatalf("code = %q, want assertion_failed", code)
	}
}

// --- verify plumbing units ---

func TestVerifySpecValidation(t *testing.T) {
	for _, p := range []map[string]any{
		{"verify": "yes"},
		{"verify": true, "verify_threshold": 0},
		{"verify": true, "verify_threshold": 2},
		{"verify": true, "verify_settle_ms": -1},
		{"verify": true, "verify_masks": "nope"},
		{"verify": true, "verify_masks": []any{map[string]any{"x": -1, "y": 0, "w": 5, "h": 5}}},
		{"verify_threshold": 0.5}, // requires verify: true
	} {
		if _, err := verifySpecFromPayload(p); err == nil {
			t.Fatalf("payload %v: want error", p)
		}
	}
	s, err := verifySpecFromPayload(map[string]any{"verify": true})
	if err != nil || !s.enabled || s.threshold != defaultVerifyThreshold || s.settle != defaultVerifySettle {
		t.Fatalf("spec = %+v, err = %v", s, err)
	}
}

func TestVerifyPrecheckRejectsBeforeCapture(t *testing.T) {
	f := newFakeRunner()
	b := testBackend(f)
	_, err := dispatch(t, b, "tap", map[string]any{"verify": true}) // no x/y
	if code := actionCode(t, err); code != "bad_request" {
		t.Fatalf("code = %q, want bad_request", code)
	}
	for _, argv := range f.argvs() {
		if strings.Contains(argv, "screenshot") {
			t.Fatalf("capture ran before payload validation: %v", argv)
		}
	}
}

func TestVerifyDeltaMasks(t *testing.T) {
	pre := image.NewGray(image.Rect(0, 0, 100, 100))
	post := image.NewGray(image.Rect(0, 0, 100, 100))
	// Change one region only.
	for y := 10; y < 30; y++ {
		for x := 10; x < 30; x++ {
			post.SetGray(x, y, color.Gray{255})
		}
	}
	if d := verifyDelta(pre, post, nil); d <= 0 {
		t.Fatalf("unmasked delta = %v, want > 0", d)
	}
	if d := verifyDelta(pre, post, []Frame{{X: 10, Y: 10, W: 20, H: 20}}); d != 0 {
		t.Fatalf("masked delta = %v, want 0", d)
	}
}
