package mockapp

import (
	"encoding/json"
	"strings"
	"testing"
)

// tree decodes describe-ui output into the raw JSON shape for assertions.
func tree(t *testing.T, a *App) []map[string]any {
	t.Helper()
	raw, err := a.DescribeUI()
	if err != nil {
		t.Fatal(err)
	}
	var roots []map[string]any
	if err := json.Unmarshal(raw, &roots); err != nil {
		t.Fatal(err)
	}
	return roots
}

// find returns the child element with the given AXUniqueId, or nil.
func find(t *testing.T, a *App, id string) map[string]any {
	t.Helper()
	roots := tree(t, a)
	if len(roots) != 1 {
		t.Fatalf("want 1 root, got %d", len(roots))
	}
	children, _ := roots[0]["children"].([]any)
	for _, c := range children {
		m, _ := c.(map[string]any)
		if m["AXUniqueId"] == id {
			return m
		}
	}
	return nil
}

func frameY(t *testing.T, el map[string]any) float64 {
	t.Helper()
	f, _ := el["frame"].(map[string]any)
	y, _ := f["y"].(float64)
	return y
}

func TestInitialTree(t *testing.T) {
	a := NewApp()
	for _, id := range []string{"title", "username", "password", "wifi", "sign-in", "reset", "footer"} {
		if find(t, a, id) == nil {
			t.Errorf("initial tree is missing element %q", id)
		}
	}
	if find(t, a, "status") != nil {
		t.Error("status label should be hidden initially")
	}
	if find(t, a, "keyboard") != nil {
		t.Error("keyboard should be hidden with no focus")
	}
	if y := frameY(t, find(t, a, "footer")); y < ScreenH {
		t.Errorf("footer should start below the fold, got y=%v", y)
	}
}

func TestTapFieldFocusesAndTypeEdits(t *testing.T) {
	a := NewApp()
	a.Tap(100, 180) // username field
	if find(t, a, "keyboard") == nil {
		t.Fatal("keyboard should appear after tapping a text field")
	}
	a.Type("ivan")
	if v := find(t, a, "username")["AXValue"]; v != "ivan" {
		t.Fatalf("username value = %v, want ivan", v)
	}
	a.Tap(100, 246) // password field
	a.Type("secret")
	if v := find(t, a, "password")["AXValue"]; v != "******" {
		t.Fatalf("password value = %v, want masked", v)
	}
	// Tapping empty space (outside the keyboard) dismisses focus.
	a.Tap(10, 60)
	if find(t, a, "keyboard") != nil {
		t.Fatal("keyboard should dismiss on a tap outside any field")
	}
}

func TestTypeWithoutFocusIsLost(t *testing.T) {
	a := NewApp()
	a.Type("nope")
	if v, ok := find(t, a, "username")["AXValue"]; ok && v != "" {
		t.Fatalf("unfocused typing must not land in a field, got %v", v)
	}
}

func TestSwitchToggles(t *testing.T) {
	a := NewApp()
	if v := find(t, a, "wifi")["AXValue"]; v != "0" {
		t.Fatalf("switch initial value = %v, want 0", v)
	}
	a.Tap(40, 320)
	if v := find(t, a, "wifi")["AXValue"]; v != "1" {
		t.Fatalf("switch value after tap = %v, want 1", v)
	}
	a.Tap(40, 320)
	if v := find(t, a, "wifi")["AXValue"]; v != "0" {
		t.Fatalf("switch value after second tap = %v, want 0", v)
	}
}

func TestSignInTransitions(t *testing.T) {
	a := NewApp()
	a.Tap(100, 390) // Sign In with empty fields
	if l, _ := find(t, a, "status")["AXLabel"].(string); l != "Missing credentials" {
		t.Fatalf("status = %q, want Missing credentials", l)
	}
	a.Tap(100, 180)
	a.Type("ivan")
	a.Tap(100, 246)
	a.Type("pw")
	a.Tap(100, 390)
	if l, _ := find(t, a, "status")["AXLabel"].(string); !strings.Contains(l, "Welcome, ivan!") {
		t.Fatalf("status = %q, want welcome", l)
	}
	// Reset restores the initial state.
	a.Tap(100, 510)
	if find(t, a, "status") != nil {
		t.Fatal("status should clear after Reset")
	}
	if v := find(t, a, "username")["AXValue"]; v != nil && v != "" {
		t.Fatalf("username should clear after Reset, got %v", v)
	}
}

func TestSwipeScrollsFooterIntoView(t *testing.T) {
	a := NewApp()
	a.Swipe(195, 590, 195, 253) // drag up ~337pt: reveal content below
	if y := frameY(t, find(t, a, "footer")); y >= ScreenH {
		t.Fatalf("footer should be on screen after scrolling, got y=%v", y)
	}
	// Scrolling is clamped: a huge downward drag returns to the top.
	a.Swipe(195, 100, 195, 5000)
	if y := frameY(t, find(t, a, "title")); y != 80 {
		t.Fatalf("title should be back at y=80 after clamped scroll, got y=%v", y)
	}
}

func TestLaunchResetsState(t *testing.T) {
	a := NewApp()
	a.Tap(100, 180)
	a.Type("ivan")
	pid1 := a.Launch()
	if v, ok := find(t, a, "username")["AXValue"]; ok && v != "" {
		t.Fatalf("launch should reset the screen, username = %v", v)
	}
	if pid2 := a.Launch(); pid2 == pid1 {
		t.Fatalf("launch pids should differ, got %d twice", pid1)
	}
}
