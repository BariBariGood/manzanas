package mockapp

import (
	"context"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/actions"
	"github.com/BariBariGood/manzanas/internal/templates"
	"github.com/BariBariGood/manzanas/proto"
)

// End-to-end template-matching tests through the full mock backend: the
// mock app renders real pixels, so save_template -> tap_template runs
// the exact production capture/match/tap paths and the state change is
// observable in the synthetic tree.

// templateBackend is a mock backend wired to a fresh on-disk template
// library.
func templateBackend(t *testing.T, store *Store) *actions.AXeBackend {
	t.Helper()
	ts, err := templates.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := NewBackend(store)
	b.SetTemplateStore(ts)
	return b
}

func backendDispatch(t *testing.T, b *actions.AXeBackend, kind string, payload map[string]any) proto.ActionResult {
	t.Helper()
	res, err := b.Dispatch(context.Background(), testUDID, proto.ActionRequest{Kind: kind, Payload: payload})
	if err != nil {
		t.Fatalf("%s: %v", kind, err)
	}
	return res
}

// save_template of the Wi-Fi switch region, then tap_template by name:
// the tap lands on the switch centre and the toggle flips (value "1").
func TestSaveTemplateThenTapTemplate(t *testing.T) {
	store := NewStore()
	b := templateBackend(t, store)
	// The Wi-Fi switch content region: (20, 300) 60x44.
	backendDispatch(t, b, "save_template", map[string]any{
		"name": "wifi-switch",
		"rect": map[string]any{"x": 20, "y": 300, "w": 60, "h": 44},
	})
	res := backendDispatch(t, b, "tap_template", map[string]any{"name": "wifi-switch"})
	if res.Result["matched"] != true {
		t.Fatalf("result = %v", res.Result)
	}
	if x, y := res.Result["x"].(float64), res.Result["y"].(float64); x != 50 || y != 322 {
		t.Fatalf("tapped (%v, %v), want the switch centre (50, 322)", x, y)
	}
	if s := res.Result["score"].(float64); s < 0.99 {
		t.Fatalf("score = %v, want ~1 on identical pixels", s)
	}
	// The tap flipped the switch: its tree value became "1".
	tree := mustJSON(t, backendDispatch(t, b, "observe", nil).Result["tree"])
	if !strings.Contains(tree, `"1"`) {
		t.Fatalf("switch did not flip: %s", tree)
	}
}

// tap_template with an unknown name fails before any capture-and-match
// loop burns the budget on a template that cannot exist.
func TestTapTemplateUnknownNameNotFound(t *testing.T) {
	store := NewStore()
	b := templateBackend(t, store)
	_, err := b.Dispatch(context.Background(), testUDID, proto.ActionRequest{
		Kind: "tap_template", Payload: map[string]any{"name": "nope", "timeout_ms": 300}})
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("err = %v, want not_found", err)
	}
}

// Without a configured template library the action is rejected before
// any capture work.
func TestTapTemplateWithoutStoreUnavailable(t *testing.T) {
	store := NewStore()
	_, err := NewBackend(store).Dispatch(context.Background(), testUDID, proto.ActionRequest{
		Kind: "tap_template", Payload: map[string]any{"name": "wifi-switch"}})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("err = %v, want unavailable", err)
	}
}

// The resolver ladder's template rung: a matcher label that the a11y
// tree cannot resolve falls back to the saved template of the same
// name, taps it, and journals resolver:"template".
func TestTapElementFallsBackToTemplateRung(t *testing.T) {
	store := NewStore()
	b := templateBackend(t, store)
	backendDispatch(t, b, "save_template", map[string]any{
		"name": "wifi-switch",
		"rect": map[string]any{"x": 20, "y": 300, "w": 60, "h": 44},
	})
	// No tree node is labelled "wifi-switch": the AX rung misses every
	// poll and the template rung resolves it.
	res := backendDispatch(t, b, "tap_element", map[string]any{
		"label": "wifi-switch", "timeout_ms": 3000, "interval_ms": 100,
	})
	if res.Result["resolver"] != "template" {
		t.Fatalf("resolver = %v, want template", res.Result["resolver"])
	}
	if x, y := res.Result["x"].(float64), res.Result["y"].(float64); x != 50 || y != 322 {
		t.Fatalf("tapped (%v, %v), want the switch centre (50, 322)", x, y)
	}
	tree := mustJSON(t, backendDispatch(t, b, "observe", nil).Result["tree"])
	if !strings.Contains(tree, `"1"`) {
		t.Fatalf("switch did not flip: %s", tree)
	}
}

// Without a saved template the ladder still resolves normal labels
// through the AX rung (the template rung must not shadow it).
func TestTapElementAXStillFirstWithTemplateRung(t *testing.T) {
	store := NewStore()
	b := templateBackend(t, store)
	res := backendDispatch(t, b, "tap_element", map[string]any{"label": "Sign In"})
	if res.Result["resolver"] != "ax" {
		t.Fatalf("resolver = %v, want ax", res.Result["resolver"])
	}
}
