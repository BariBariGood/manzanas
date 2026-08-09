package actions

import (
	"strings"
	"testing"
)

func offscreenFixture() []*Node {
	return []*Node{
		anode("Button", "Visible", 20, 100, 100, 44, true),
		anode("Button", "Below", 20, 1200, 100, 44, true),  // below the fold
		anode("Button", "Above", 20, -200, 100, 44, true),  // scrolled past
		anode("Cell", "AlsoBelow", 0, 1300, 393, 46, true), // below the fold
	}
}

func TestOffscreenHintFlatPredicate(t *testing.T) {
	pr := predicate{Label: "Below", Exact: true}
	hint := offscreenHint(pr, offscreenFixture(), auditViewport)
	if !strings.Contains(hint, "1 matching element(s) exist off-screen") {
		t.Fatalf("hint = %q", hint)
	}
	if !strings.Contains(hint, "below the viewport") || !strings.Contains(hint, "scroll_to_element") {
		t.Fatalf("hint = %q", hint)
	}
	if hint := offscreenHint(predicate{Label: "Visible"}, offscreenFixture(), auditViewport); hint != "" {
		t.Fatalf("on-screen match should yield no hint, got %q", hint)
	}
	if hint := offscreenHint(predicate{Label: "Nowhere"}, offscreenFixture(), auditViewport); hint != "" {
		t.Fatalf("no candidates should yield no hint, got %q", hint)
	}
}

func TestOffscreenHintFlatInFrame(t *testing.T) {
	// The element is on screen but outside the requested in_frame region.
	pr := predicate{Label: "Visible", InFrame: &Frame{X: 0, Y: 400, W: 393, H: 400}}
	hint := offscreenHint(pr, offscreenFixture(), auditViewport)
	if !strings.Contains(hint, "on screen but outside the requested in_frame/bounds_hint region") ||
		!strings.Contains(hint, "relax or adjust the region") {
		t.Fatalf("hint = %q", hint)
	}
	if strings.Contains(hint, "scroll_to_element") {
		t.Fatalf("region-excluded candidates must not get scroll advice: %q", hint)
	}
}

func TestOffscreenHintDSL(t *testing.T) {
	d, err := dslFromMap(map[string]any{"text": "Above"}, 0)
	if err != nil {
		t.Fatalf("dsl: %v", err)
	}
	hint := offscreenHint(d, offscreenFixture(), auditViewport)
	if !strings.Contains(hint, "above the viewport") {
		t.Fatalf("hint = %q", hint)
	}
}

func TestOffscreenHintDeterministicAndCapped(t *testing.T) {
	var nodes []*Node
	for i := 0; i < 6; i++ {
		nodes = append(nodes, anode("Button", "Row", 20, 1000+float64(i)*50, 100, 44, true))
	}
	pr := predicate{Label: "Row"}
	first := offscreenHint(pr, nodes, auditViewport)
	if !strings.Contains(first, "6 matching element(s)") || !strings.Contains(first, "and 3 more") {
		t.Fatalf("hint = %q", first)
	}
	for i := 0; i < 3; i++ {
		if offscreenHint(pr, nodes, auditViewport) != first {
			t.Fatalf("hint is not deterministic")
		}
	}
}

func TestAmbiguousMatchMarksOffscreen(t *testing.T) {
	nodes := []*Node{
		anode("Button", "Dup", 20, 100, 100, 44, true),
		anode("Button", "Dup", 20, 1200, 100, 44, true),
	}
	d, err := dslFromMap(map[string]any{"text": "Dup"}, 0)
	if err != nil {
		t.Fatalf("dsl: %v", err)
	}
	_, rerr := d.resolve(nodes, auditViewport)
	if rerr == nil {
		t.Fatalf("duplicate match should be ambiguous")
	}
	msg := rerr.Error()
	if !strings.Contains(msg, "(off-screen)") || !strings.Contains(msg, "scroll_to_element") {
		t.Fatalf("ambiguous error missing off-screen marker: %q", msg)
	}
}

func TestOffscreenNilViewport(t *testing.T) {
	// Without a viewport nothing can be called off-screen.
	if hint := offscreenHint(predicate{Label: "Below"}, offscreenFixture(), nil); hint != "" {
		t.Fatalf("nil viewport should yield no hint, got %q", hint)
	}
}
