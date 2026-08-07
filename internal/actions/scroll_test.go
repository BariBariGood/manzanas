package actions

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// vpTreeJSON builds a describe-ui document with a 400x800 screen frame and
// one cell per label at the given y offsets, so scroll tests can place
// elements inside or below the viewport.
func vpTreeJSON(cells ...[2]any) string {
	parts := make([]string, 0, len(cells))
	for _, c := range cells {
		parts = append(parts, fmt.Sprintf(
			`{"role":"Cell","AXLabel":%q,"frame":{"x":0,"y":%v,"width":400,"height":44}}`, c[0], c[1]))
	}
	return `[{"role":"Window","frame":{"x":0,"y":0,"width":400,"height":800},"children":[` +
		strings.Join(parts, ",") + `]}]`
}

func fastScroll(extra map[string]any) map[string]any {
	p := map[string]any{"timeout_ms": 2000.0, "interval_ms": 10.0}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func TestScrollToElementAlreadyVisible(t *testing.T) {
	b, r := compositeBackend(seqStep{stdout: vpTreeJSON([2]any{"Settings", 100})})
	res, err := handleScrollToElement(context.Background(), b, "UDID",
		fastScroll(map[string]any{"label": "Settings"}))
	if err != nil {
		t.Fatalf("scroll_to_element: %v", err)
	}
	if res["scrolls"] != 0 {
		t.Fatalf("scrolls = %v, want 0", res["scrolls"])
	}
	el, ok := res["element"].(*Node)
	if !ok || el.Label != "Settings" || el.Frame == nil {
		t.Fatalf("bad element: %+v", res["element"])
	}
	if h, _ := res["hash"].(string); h == "" {
		t.Fatalf("missing hash in result: %v", res)
	}
	if swipe := lastCall(r, "swipe"); swipe != nil {
		t.Fatalf("unexpected swipe for a visible element: %v", swipe)
	}
}

func TestScrollToElementSwipesTowardOffViewportMatch(t *testing.T) {
	b, r := compositeBackend(
		seqStep{stdout: vpTreeJSON([2]any{"Filler", 100}, [2]any{"Privacy", 1200})},
		seqStep{stdout: vpTreeJSON([2]any{"Privacy", 400})},
	)
	res, err := handleScrollToElement(context.Background(), b, "UDID",
		fastScroll(map[string]any{"label": "Privacy"}))
	if err != nil {
		t.Fatalf("scroll_to_element: %v", err)
	}
	if res["scrolls"] != 1 {
		t.Fatalf("scrolls = %v, want 1", res["scrolls"])
	}
	swipe := lastCall(r, "swipe")
	if swipe == nil {
		t.Fatal("no swipe invocation recorded")
	}
	// Element below the viewport: swipe drags upward (start-y > end-y).
	want := []string{"swipe", "--start-x", "200", "--start-y", "560", "--end-x", "200", "--end-y", "240"}
	if !hasArgs(swipe, want) {
		t.Fatalf("swipe args = %v, want %v", swipe, want)
	}
}

func TestScrollToElementNotFoundIsTimeout(t *testing.T) {
	b, _ := compositeBackend(seqStep{stdout: vpTreeJSON([2]any{"Filler", 100})})
	_, err := handleScrollToElement(context.Background(), b, "UDID",
		fastScroll(map[string]any{"label": "Missing", "max_scrolls": 2.0}))
	e, ok := err.(*Error)
	if !ok || e.Code != "timeout" {
		t.Fatalf("err = %v, want timeout code", err)
	}
	if !strings.Contains(e.Message, "not found in the accessibility tree") {
		t.Fatalf("message should say the element was never in the tree: %s", e.Message)
	}
}

func TestScrollToElementStuckOffViewportIsOffViewport(t *testing.T) {
	b, _ := compositeBackend(seqStep{stdout: vpTreeJSON([2]any{"Pinned", 1200})})
	_, err := handleScrollToElement(context.Background(), b, "UDID",
		fastScroll(map[string]any{"label": "Pinned", "max_scrolls": 2.0}))
	e, ok := err.(*Error)
	if !ok || e.Code != "off_viewport" {
		t.Fatalf("err = %v, want off_viewport code", err)
	}
	if !strings.Contains(e.Message, "could not be brought into the viewport") {
		t.Fatalf("message should say the element was matched but stayed off screen: %s", e.Message)
	}
}

func TestScrollToElementNoFrameFailsFast(t *testing.T) {
	b, r := compositeBackend(seqStep{stdout: `[{"role":"Window","frame":{"x":0,"y":0,"width":400,"height":800},"children":[{"role":"Cell","AXLabel":"Ghost"}]}]`})
	_, err := handleScrollToElement(context.Background(), b, "UDID",
		fastScroll(map[string]any{"label": "Ghost", "max_scrolls": 2.0}))
	e, ok := err.(*Error)
	if !ok || e.Code != "bad_request" {
		t.Fatalf("err = %v, want bad_request", err)
	}
	if !strings.Contains(e.Message, "no usable frame") {
		t.Fatalf("message should say the frame is unusable: %s", e.Message)
	}
	if swipe := lastCall(r, "swipe"); swipe != nil {
		t.Fatalf("unexpected swipe for a frameless match: %v", swipe)
	}
}

func TestScrollToElementRejectsBadDirection(t *testing.T) {
	b, _ := compositeBackend(seqStep{stdout: vpTreeJSON([2]any{"Filler", 100})})
	_, err := handleScrollToElement(context.Background(), b, "UDID",
		fastScroll(map[string]any{"label": "Filler", "direction": "sideways"}))
	if e, ok := err.(*Error); !ok || e.Code != "bad_request" {
		t.Fatalf("err = %v, want bad_request", err)
	}
}

func TestScrollToElementRequiresPredicate(t *testing.T) {
	b, _ := compositeBackend(seqStep{stdout: vpTreeJSON([2]any{"Filler", 100})})
	_, err := handleScrollToElement(context.Background(), b, "UDID", map[string]any{})
	if e, ok := err.(*Error); !ok || e.Code != "bad_request" {
		t.Fatalf("err = %v, want bad_request", err)
	}
}
