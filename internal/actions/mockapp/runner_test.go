package mockapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

const testUDID = "MOCK-0000-0000-0001"

// dispatch runs one action through the full mock backend (real AXeBackend
// handlers over the mock Runner).
func dispatch(t *testing.T, store *Store, kind string, payload map[string]any) proto.ActionResult {
	t.Helper()
	res, err := NewBackend(store).Dispatch(context.Background(), testUDID, proto.ActionRequest{Kind: kind, Payload: payload})
	if err != nil {
		t.Fatalf("%s: %v", kind, err)
	}
	return res
}

func TestObserveCompactsSyntheticTree(t *testing.T) {
	store := NewStore()
	res := dispatch(t, store, "observe", nil)
	if res.Result["hash"] == "" {
		t.Fatal("observe should return a tree hash")
	}
	tree := mustJSON(t, res.Result["tree"])
	for _, want := range []string{"Mock Login", "Username", "Sign In", "footer"} {
		if !strings.Contains(tree, want) {
			t.Errorf("observe tree is missing %q", want)
		}
	}
}

func TestTapElementByLabelTogglesSwitch(t *testing.T) {
	store := NewStore()
	dispatch(t, store, "tap_element", map[string]any{"label": "Wi-Fi", "role": "Switch"})
	res := dispatch(t, store, "wait_for_element", map[string]any{"id": "wifi", "value": "1", "timeout_ms": 2000})
	if res.Result["element"] == nil {
		t.Fatal("switch should read 1 after tap_element")
	}
}

func TestTypeIntoElementAndWaitForWelcome(t *testing.T) {
	store := NewStore()
	dispatch(t, store, "type_into_element", map[string]any{"id": "username", "text": "ivan", "require_focus": true})
	dispatch(t, store, "type_into_element", map[string]any{"id": "password", "text": "pw"})
	dispatch(t, store, "tap_element", map[string]any{"label": "Sign In"})
	res := dispatch(t, store, "wait_for_element", map[string]any{"label": "Welcome, ivan!", "timeout_ms": 2000})
	if res.Result["element"] == nil {
		t.Fatal("welcome label should appear after sign in")
	}
}

func TestPasteStrategyDeliversPasteboard(t *testing.T) {
	store := NewStore()
	dispatch(t, store, "tap_element", map[string]any{"id": "username"})
	dispatch(t, store, "type", map[string]any{"text": "pasted!", "strategy": "paste"})
	res := dispatch(t, store, "wait_for_element", map[string]any{"id": "username", "value": "pasted!", "timeout_ms": 2000})
	if res.Result["element"] == nil {
		t.Fatal("paste strategy should land the text in the focused field")
	}
	pb := dispatch(t, store, "pasteboard_get", nil)
	if pb.Result["text"] != "pasted!" {
		t.Fatalf("pasteboard = %v, want pasted!", pb.Result["text"])
	}
}

func TestScrollToElementReachesFooter(t *testing.T) {
	store := NewStore()
	res := dispatch(t, store, "scroll_to_element", map[string]any{"id": "footer", "timeout_ms": 10000})
	if res.Result["element"] == nil {
		t.Fatal("scroll_to_element should bring the footer into the viewport")
	}
	if n, _ := res.Result["scrolls"].(int); n == 0 {
		t.Fatalf("scroll_to_element should have scrolled at least once, got %v", res.Result["scrolls"])
	}
}

func TestScreenshotReturnsValidPNG(t *testing.T) {
	store := NewStore()
	res := dispatch(t, store, "screenshot", nil)
	b64, _ := res.Result["png_base64"].(string)
	img, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(img) == 0 {
		t.Fatalf("screenshot should return base64 PNG bytes: %v", err)
	}
	if string(img[1:4]) != "PNG" {
		t.Fatal("screenshot bytes are not a PNG")
	}
}

func TestAuditRunsAgainstSyntheticTree(t *testing.T) {
	store := NewStore()
	res := dispatch(t, store, "audit", nil)
	if _, ok := res.Result["findings"]; !ok {
		t.Fatal("audit should return findings over the synthetic tree")
	}
	if res.Result["png_base64"] == nil {
		t.Fatal("audit should return the annotated screenshot")
	}
}

func TestLaunchAppResetsAndReturnsPID(t *testing.T) {
	store := NewStore()
	dispatch(t, store, "type_into_element", map[string]any{"id": "username", "text": "ivan"})
	res := dispatch(t, store, "launch_app", map[string]any{"bundle_id": "com.example.mock"})
	if res.Result["pid"] == nil {
		t.Fatal("launch_app should return a pid")
	}
	after := dispatch(t, store, "observe", nil)
	if strings.Contains(mustJSON(t, after.Result["tree"]), "ivan") {
		t.Fatal("launch_app should reset the screen state")
	}
}

func TestNotBootedSurfacesProtocolError(t *testing.T) {
	store := NewStore()
	backend := NewBackend(store, WithBooted(func(context.Context, string) bool { return false }))
	_, err := backend.Dispatch(context.Background(), testUDID, proto.ActionRequest{Kind: "tap", Payload: map[string]any{"x": 1, "y": 2}})
	if err == nil || !strings.Contains(err.Error(), "not booted") {
		t.Fatalf("want target-not-booted error, got %v", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
