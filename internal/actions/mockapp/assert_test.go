package mockapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

// End-to-end verification-layer tests through the full mock backend:
// the real AXeBackend handlers observe and screenshot the synthetic app,
// so assert_* and verified taps run the exact production code paths.

func dispatchErr(t *testing.T, store *Store, kind string, payload map[string]any) (proto.ActionResult, error) {
	t.Helper()
	return NewBackend(store).Dispatch(context.Background(), testUDID, proto.ActionRequest{Kind: kind, Payload: payload})
}

func TestAssertTextOnLoginScreen(t *testing.T) {
	store := NewStore()
	res := dispatch(t, store, "assert_text", map[string]any{"text": "Mock Login"})
	if res.Result["found"] != true {
		t.Fatalf("result = %v", res.Result)
	}
	// Fuzzy contains + case folding by default.
	res = dispatch(t, store, "assert_text", map[string]any{"text": "sign in"})
	if res.Result["matched"] != "Sign In" {
		t.Fatalf("result = %v", res.Result)
	}
}

func TestAssertTextAbsentFailsAfterRetry(t *testing.T) {
	store := NewStore()
	_, err := dispatchErr(t, store, "assert_text", map[string]any{
		"text": "Logout", "timeout_ms": 300, "interval_ms": 50,
	})
	if err == nil || !strings.Contains(err.Error(), "assertion_failed") {
		t.Fatalf("err = %v, want assertion_failed", err)
	}
}

func TestAssertNoTextOnLoginScreen(t *testing.T) {
	store := NewStore()
	dispatch(t, store, "assert_no_text", map[string]any{"text": "Logout"})
	_, err := dispatchErr(t, store, "assert_no_text", map[string]any{
		"text": "Mock Login", "timeout_ms": 300, "interval_ms": 50,
	})
	if err == nil || !strings.Contains(err.Error(), "assertion_failed") {
		t.Fatalf("err = %v, want assertion_failed", err)
	}
}

func TestAssertTemplateOnLoginScreen(t *testing.T) {
	store := NewStore()
	frame, err := png.Decode(bytes.NewReader(renderPNG(t, store)))
	if err != nil {
		t.Fatal(err)
	}
	// Crop the Sign In button region as the template.
	sub := frame.(interface {
		SubImage(image.Rectangle) image.Image
	}).SubImage(image.Rect(20, 368, 370, 418))
	var buf bytes.Buffer
	if err := png.Encode(&buf, sub); err != nil {
		t.Fatal(err)
	}
	res := dispatch(t, store, "assert_template", map[string]any{
		"template_base64": base64.StdEncoding.EncodeToString(buf.Bytes()),
	})
	if res.Result["found"] != true {
		t.Fatalf("result = %v", res.Result)
	}
	if x, y := res.Result["x"].(int), res.Result["y"].(int); x != 20 || y != 368 {
		t.Fatalf("matched at (%d, %d), want (20, 368)", x, y)
	}
}

func renderPNG(t *testing.T, store *Store) []byte {
	t.Helper()
	raw, err := store.App(testUDID).RenderPNG()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestVerifiedTapOnToggle(t *testing.T) {
	store := NewStore()
	// The Wi-Fi switch centre: content (20,300) 60x44 => (50, 322).
	res := dispatch(t, store, "tap", map[string]any{
		"x": 50, "y": 322, "verify": true, "verify_settle_ms": 10,
	})
	if res.Result["verified"] != true {
		t.Fatalf("result = %v, want verified:true (pixel_delta %v)", res.Result, res.Result["pixel_delta"])
	}
	// The flip is also visible in the tree: the switch value became "1".
	tree := mustJSON(t, dispatch(t, store, "observe", nil).Result["tree"])
	if !strings.Contains(tree, `"1"`) {
		t.Fatalf("switch did not flip: %s", tree)
	}
}

func TestVerifiedNoopTap(t *testing.T) {
	store := NewStore()
	// A tap on empty background with no focus to dismiss: nothing changes.
	res := dispatch(t, store, "tap", map[string]any{
		"x": 380, "y": 700, "verify": true, "verify_settle_ms": 10,
	})
	if res.Result["verified"] != false {
		t.Fatalf("result = %v, want verified:false", res.Result)
	}
	if d, ok := res.Result["pixel_delta"].(float64); !ok || d != 0 {
		t.Fatalf("pixel_delta = %v, want 0", res.Result["pixel_delta"])
	}
}

func TestVerifiedTapMaskedToggle(t *testing.T) {
	store := NewStore()
	// Masking the switch region hides the only change: verified:false.
	res := dispatch(t, store, "tap", map[string]any{
		"x": 50, "y": 322, "verify": true, "verify_settle_ms": 10,
		"verify_masks": []any{map[string]any{"x": 0, "y": 280, "w": 390, "h": 90}},
	})
	if res.Result["verified"] != false {
		t.Fatalf("result = %v, want verified:false with the change masked", res.Result)
	}
}
