package actions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// recSeqRunner wraps seqRunner, recording every invocation's argv so
// composite tests can assert on the tap/type calls that follow the find.
type recSeqRunner struct {
	seq *seqRunner

	mu    sync.Mutex
	calls [][]string
}

func (r *recSeqRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	return r.seq.Run(ctx, name, args...)
}

func compositeBackend(steps ...seqStep) (*AXeBackend, *recSeqRunner) {
	r := &recSeqRunner{seq: &seqRunner{outputs: steps}}
	return NewAXe(WithRunner(r), WithAXePath("/fake/axe")), r
}

func TestTapElementTapsCenter(t *testing.T) {
	b, r := compositeBackend(seqStep{stdout: treeJSON("Settings", "General")})
	res, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "General"}))
	if err != nil {
		t.Fatalf("tap_element: %v", err)
	}
	// treeJSON's second cell: frame x=0 y=144 w=400 h=44 -> center (200, 166).
	if res["x"] != 200.0 || res["y"] != 166.0 {
		t.Fatalf("center = (%v, %v), want (200, 166)", res["x"], res["y"])
	}
	el, ok := res["element"].(*Node)
	if !ok || el.Label != "General" {
		t.Fatalf("bad element: %+v", res["element"])
	}
	tap := lastCall(r, "tap")
	if tap == nil {
		t.Fatal("no tap invocation recorded")
	}
	if want := []string{"tap", "-x", "200", "-y", "166"}; !hasArgs(tap, want) {
		t.Fatalf("tap args = %v, want %v", tap, want)
	}
}

func TestTapElementWaitsForElement(t *testing.T) {
	b, _ := seqBackend(
		seqStep{stdout: treeJSON("Settings")},
		seqStep{stdout: treeJSON("Settings", "General")},
	)
	res, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "General"}))
	if err != nil {
		t.Fatalf("tap_element: %v", err)
	}
	if polls, _ := res["polls"].(int); polls < 2 {
		t.Fatalf("polls = %v, want >= 2", res["polls"])
	}
}

func TestTapElementTimesOut(t *testing.T) {
	b, _ := seqBackend(seqStep{stdout: treeJSON("Settings")})
	_, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "Missing"}))
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	if e, ok := err.(*Error); !ok || e.Code != "timeout" {
		t.Fatalf("err = %v, want timeout code", err)
	}
}

func TestTapElementRequiresPredicate(t *testing.T) {
	b, _ := seqBackend(seqStep{stdout: treeJSON("Settings")})
	_, err := handleTapElement(context.Background(), b, "UDID", map[string]any{})
	if e, ok := err.(*Error); !ok || e.Code != "bad_request" {
		t.Fatalf("err = %v, want bad_request", err)
	}
}

func TestTypeIntoElementTapsThenTypes(t *testing.T) {
	b, r := compositeBackend(seqStep{stdout: treeJSON("Search")})
	res, err := handleTypeIntoElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "Search", "text": "hello"}))
	if err != nil {
		t.Fatalf("type_into_element: %v", err)
	}
	if res["typed_runes"] != 5 {
		t.Fatalf("typed_runes = %v, want 5", res["typed_runes"])
	}
	tapIdx, typeIdx := -1, -1
	r.mu.Lock()
	for i, c := range r.calls {
		if len(c) > 1 && c[1] == "tap" {
			tapIdx = i
		}
		if len(c) > 1 && c[1] == "type" {
			typeIdx = i
		}
	}
	r.mu.Unlock()
	if tapIdx < 0 || typeIdx < 0 || tapIdx > typeIdx {
		t.Fatalf("want tap before type, got tap=%d type=%d", tapIdx, typeIdx)
	}
}

func TestTypeIntoElementRequiresText(t *testing.T) {
	b, _ := seqBackend(seqStep{stdout: treeJSON("Search")})
	_, err := handleTypeIntoElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "Search"}))
	if e, ok := err.(*Error); !ok || e.Code != "bad_request" {
		t.Fatalf("err = %v, want bad_request", err)
	}
}

func TestTapElementUsesWarmHID(t *testing.T) {
	b, r := compositeBackend(seqStep{stdout: treeJSON("Settings", "General")})
	var warm [][]any
	b.SetWarmHID(func(_ context.Context, udid, op string, args map[string]any) error {
		warm = append(warm, []any{udid, op, args["x"], args["y"]})
		return nil
	})
	res, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "General"}))
	if err != nil {
		t.Fatalf("tap_element: %v", err)
	}
	if len(warm) != 1 || warm[0][1] != "tap" || warm[0][2] != 200.0 || warm[0][3] != 166.0 {
		t.Fatalf("warm HID calls = %v", warm)
	}
	if tap := lastCall(r, "tap"); tap != nil {
		t.Fatalf("cold tap CLI was invoked: %v", tap)
	}
	if res["x"] != 200.0 || res["y"] != 166.0 {
		t.Fatalf("center = (%v, %v), want (200, 166)", res["x"], res["y"])
	}
}

func TestTapElementWarmHIDTransportFallsBackCold(t *testing.T) {
	b, r := compositeBackend(seqStep{stdout: treeJSON("Settings", "General")})
	b.SetWarmHID(func(context.Context, string, string, map[string]any) error {
		return &TransportError{Err: context.Canceled}
	})
	if _, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "General"})); err != nil {
		t.Fatalf("tap_element: %v", err)
	}
	if tap := lastCall(r, "tap"); tap == nil {
		t.Fatal("cold tap CLI fallback not invoked")
	}
}

func TestTapElementWarmHIDDeliveredFailureSurfaces(t *testing.T) {
	b, r := compositeBackend(seqStep{stdout: treeJSON("Settings", "General")})
	b.SetWarmHID(func(context.Context, string, string, map[string]any) error {
		return &TransportError{Err: context.Canceled, Delivered: true}
	})
	_, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "General"}))
	if e, ok := err.(*Error); !ok || e.Code != "unavailable" {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if tap := lastCall(r, "tap"); tap != nil {
		t.Fatalf("delivered failure must not retry cold, got %v", tap)
	}
}

func TestTypeIntoElementWarmOnlyBackend(t *testing.T) {
	// No AXe binary at all: observe and both HID inputs (tap, type) are
	// serviced entirely by the warm helper.
	b := NewAXe(WithRunner(newFakeRunner()), WithAXePath(""))
	nodes, err := CompactTree([]byte(treeJSON("Search")))
	if err != nil {
		t.Fatalf("CompactTree: %v", err)
	}
	b.SetWarmObserver(func(context.Context, string) (observation, error) {
		return observation{nodes: nodes}, nil
	})
	var ops []string
	b.SetWarmHID(func(_ context.Context, _, op string, args map[string]any) error {
		ops = append(ops, op)
		if op == "type" && args["text"] != "hello" {
			t.Errorf("warm type text = %v, want hello", args["text"])
		}
		return nil
	})
	res, err := handleTypeIntoElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "Search", "text": "hello"}))
	if err != nil {
		t.Fatalf("type_into_element: %v", err)
	}
	if res["typed_runes"] != 5 {
		t.Fatalf("typed_runes = %v, want 5", res["typed_runes"])
	}
	if len(ops) != 2 || ops[0] != "tap" || ops[1] != "type" {
		t.Fatalf("warm HID ops = %v, want [tap type]", ops)
	}
}

func TestTypeIntoElementWarmUnknownOpTypesCold(t *testing.T) {
	// A helper predating the "type" op (hidOp maps its "unknown op"
	// rejection to errColdOnly): the tap stays warm, the typing runs on
	// the cold CLI.
	b, r := compositeBackend(seqStep{stdout: treeJSON("Search")})
	b.SetWarmHID(func(_ context.Context, _, op string, _ map[string]any) error {
		if op == "type" {
			return errColdOnly
		}
		return nil
	})
	res, err := handleTypeIntoElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "Search", "text": "hello"}))
	if err != nil {
		t.Fatalf("type_into_element: %v", err)
	}
	if res["typed_runes"] != 5 {
		t.Fatalf("typed_runes = %v, want 5", res["typed_runes"])
	}
	if c := lastCall(r, "type"); c == nil {
		t.Fatal("cold type CLI fallback not invoked")
	}
	if c := lastCall(r, "tap"); c != nil {
		t.Fatalf("tap should have stayed warm, got cold %v", c)
	}
}

// viewportTreeJSON builds a describe-ui document whose root Application
// carries the 400x874 screen frame, with one 44pt cell per label at the
// given y offsets.
func viewportTreeJSON(labels []string, ys []int) string {
	cells := make([]string, 0, len(labels))
	for i, l := range labels {
		cells = append(cells, fmt.Sprintf(
			`{"role":"Cell","AXLabel":%q,"frame":{"x":0,"y":%d,"width":400,"height":44}}`, l, ys[i]))
	}
	return `[{"role":"Application","frame":{"x":0,"y":0,"width":400,"height":874},"children":[` +
		strings.Join(cells, ",") + `]}]`
}

func TestTapElementAnchors(t *testing.T) {
	// treeJSON's second cell: frame x=0 y=144 w=400 h=44; inset = h/2 = 22.
	tests := []struct {
		anchor string
		wantX  float64
	}{
		{"start", 22},
		{"center", 200},
		{"end", 378},
		{"", 200},
	}
	for _, tt := range tests {
		t.Run("anchor="+tt.anchor, func(t *testing.T) {
			b, r := compositeBackend(seqStep{stdout: treeJSON("Settings", "General")})
			p := fastWait(map[string]any{"label": "General"})
			if tt.anchor != "" {
				p["anchor"] = tt.anchor
			}
			res, err := handleTapElement(context.Background(), b, "UDID", p)
			if err != nil {
				t.Fatalf("tap_element: %v", err)
			}
			if res["x"] != tt.wantX || res["y"] != 166.0 {
				t.Fatalf("tap point = (%v, %v), want (%v, 166)", res["x"], res["y"], tt.wantX)
			}
			tap := lastCall(r, "tap")
			if want := []string{"tap", "-x", fmtNum(tt.wantX), "-y", "166"}; !hasArgs(tap, want) {
				t.Fatalf("tap args = %v, want %v", tap, want)
			}
		})
	}
}

func TestTapElementRejectsBadAnchor(t *testing.T) {
	b, _ := seqBackend(seqStep{stdout: treeJSON("General")})
	_, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "General", "anchor": "middle"}))
	if e, ok := err.(*Error); !ok || e.Code != "bad_request" {
		t.Fatalf("err = %v, want bad_request", err)
	}
}

func TestTapElementOffViewportErrors(t *testing.T) {
	// "Quick" sits at y=901 on an 874pt viewport: the tap must fail with
	// an explicit off_viewport error, not silently land outside the
	// screen and report ok.
	b, r := compositeBackend(seqStep{stdout: viewportTreeJSON(
		[]string{"Settings", "Quick"}, []int{100, 901})})
	_, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "Quick"}))
	if e, ok := err.(*Error); !ok || e.Code != "off_viewport" {
		t.Fatalf("err = %v, want off_viewport", err)
	}
	if tap := lastCall(r, "tap"); tap != nil {
		t.Fatalf("off-viewport element must not be tapped, got %v", tap)
	}
}

func TestTapElementWaitsForElementToSlideOnScreen(t *testing.T) {
	// A sheet still animating in: "Quick" starts below the 874pt viewport
	// and lands on screen a poll later. The wait budget must be used
	// instead of failing off_viewport on the first poll.
	b, r := compositeBackend(
		seqStep{stdout: viewportTreeJSON([]string{"Quick"}, []int{901})},
		seqStep{stdout: viewportTreeJSON([]string{"Quick"}, []int{700})},
	)
	if _, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "Quick"})); err != nil {
		t.Fatalf("tap_element: %v", err)
	}
	if tap := lastCall(r, "tap"); tap == nil {
		t.Fatal("expected a tap once the element slid on screen")
	}
}

func TestTapElementInViewportStillTaps(t *testing.T) {
	b, r := compositeBackend(seqStep{stdout: viewportTreeJSON(
		[]string{"Settings"}, []int{100})})
	if _, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "Settings"})); err != nil {
		t.Fatalf("tap_element: %v", err)
	}
	if tap := lastCall(r, "tap"); tap == nil {
		t.Fatal("expected a tap")
	}
}

func TestTapElementRefreshBypassesWarmObserver(t *testing.T) {
	// The warm observer serves a stale tree missing the element; the cold
	// CLI sees the real screen. refresh:true must poll cold and find it.
	b, r := compositeBackend(seqStep{stdout: treeJSON("Settings", "General")})
	warmPolls := 0
	b.SetWarmObserver(func(context.Context, string) (observation, error) {
		warmPolls++
		nodes, _ := CompactTree([]byte(treeJSON("Settings")))
		return observation{nodes: nodes}, nil
	})
	res, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "General", "refresh": true}))
	if err != nil {
		t.Fatalf("tap_element with refresh: %v", err)
	}
	if warmPolls != 0 {
		t.Fatalf("refresh polled the warm observer %d time(s)", warmPolls)
	}
	if res["x"] != 200.0 || res["y"] != 166.0 {
		t.Fatalf("tap point = (%v, %v), want (200, 166)", res["x"], res["y"])
	}
	if c := lastCall(r, "describe-ui"); c == nil {
		t.Fatal("expected a cold describe-ui poll")
	}
}

func TestTapElementRejectsNonStringAnchor(t *testing.T) {
	b, _ := compositeBackend(seqStep{stdout: treeJSON("Settings")})
	_, err := handleTapElement(context.Background(), b, "UDID",
		map[string]any{"label": "Settings", "anchor": 1})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "bad_request" {
		t.Fatalf("non-string anchor: err = %v, want bad_request", err)
	}
}

func TestTapElementRefreshKeepsWarmOnHelperOnlyHost(t *testing.T) {
	// No AXe CLI on the host: refresh cannot be honored, so the warm
	// observer must still serve the poll instead of failing unavailable.
	b := NewAXe(WithRunner(&seqRunner{}), WithAXePath(""))
	b.SetWarmObserver(func(context.Context, string) (observation, error) {
		nodes, _ := CompactTree([]byte(treeJSON("Settings", "General")))
		return observation{nodes: nodes}, nil
	})
	b.SetWarmHID(func(context.Context, string, string, map[string]any) error {
		return nil
	})
	if _, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "General", "refresh": true})); err != nil {
		t.Fatalf("tap_element with refresh on helper-only host: %v", err)
	}
}

func TestTypeIntoElementByPlaceholder(t *testing.T) {
	// An RN TextInput: empty AX label, placeholder surfaced via
	// AXPlaceholderValue. The placeholder predicate must find it.
	tree := `[{"role":"Window","children":[` +
		`{"role":"TextField","AXLabel":"","AXPlaceholderValue":"Paste a recipe link",` +
		`"frame":{"x":20,"y":300,"width":350,"height":44}}]}]`
	b, _ := compositeBackend(seqStep{stdout: tree})
	res, err := handleTypeIntoElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"placeholder": "recipe link", "text": "hello"}))
	if err != nil {
		t.Fatalf("type_into_element by placeholder: %v", err)
	}
	if res["typed_runes"] != 5 {
		t.Fatalf("typed_runes = %v, want 5", res["typed_runes"])
	}
	el, ok := res["element"].(*Node)
	if !ok || el.Placeholder != "Paste a recipe link" {
		t.Fatalf("bad element: %+v", res["element"])
	}
}

func TestTapElementRejectsBadRefresh(t *testing.T) {
	b, _ := seqBackend(seqStep{stdout: treeJSON("General")})
	_, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "General", "refresh": "yes"}))
	if e, ok := err.(*Error); !ok || e.Code != "bad_request" {
		t.Fatalf("err = %v, want bad_request", err)
	}
}

// lastCall returns the last recorded invocation whose subcommand matches.
func lastCall(r *recSeqRunner, sub string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.calls) - 1; i >= 0; i-- {
		if len(r.calls[i]) > 1 && r.calls[i][1] == sub {
			return r.calls[i]
		}
	}
	return nil
}

// hasArgs reports whether call's argv contains want as a subsequence of
// adjacent entries.
func hasArgs(call, want []string) bool {
	return strings.Contains(" "+strings.Join(call, " ")+" ", " "+strings.Join(want, " ")+" ")
}
