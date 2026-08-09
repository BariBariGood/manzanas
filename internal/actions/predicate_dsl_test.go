package actions

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// dslTree is a canned compacted tree: a login-ish screen with duplicate
// buttons, labelled fields, and a list of cells, exercising every
// predicate field.
//
//	viewport 400x800
//	  StaticText "Email"          (20,100 80x20)
//	  TextField  id=email-field   (120,100 260x30)
//	  StaticText "Password"       (20,160 80x20)
//	  SecureTextField id=pw-field (120,160 260x30)
//	  Button "Save" id=save-top   (20,40 100x40)    top half
//	  Button "Save" id=save-bot   (20,700 100x40)   bottom half
//	  Cell  (0,300 400x60)
//	    StaticText "Alice"  (20,315 100x30)
//	    Button "Delete" id=del-alice (320,315 60x30)
//	  Cell  (0,360 400x60)
//	    StaticText "Bob"    (20,375 100x30)
//	    Button "Delete" id=del-bob (320,375 60x30)
func dslTree() []*Node {
	f := func(x, y, w, h float64) *Frame { return &Frame{X: x, Y: y, W: w, H: h} }
	return []*Node{{
		Role: "Window", Frame: f(0, 0, 400, 800),
		Children: []*Node{
			{Role: "StaticText", Label: "Email", Frame: f(20, 100, 80, 20)},
			{Role: "TextField", Identifier: "email-field", Frame: f(120, 100, 260, 30), Interactable: true},
			{Role: "StaticText", Label: "Password", Frame: f(20, 160, 80, 20)},
			{Role: "SecureTextField", Identifier: "pw-field", Frame: f(120, 160, 260, 30), Interactable: true},
			{Role: "Button", Label: "Save", Identifier: "save-top", Frame: f(20, 40, 100, 40), Interactable: true},
			{Role: "Button", Label: "Save", Identifier: "save-bot", Frame: f(20, 700, 100, 40), Interactable: true},
			{Role: "Cell", Frame: f(0, 300, 400, 60), Interactable: true, Children: []*Node{
				{Role: "StaticText", Label: "Alice", Frame: f(20, 315, 100, 30)},
				{Role: "Button", Label: "Delete", Identifier: "del-alice", Frame: f(320, 315, 60, 30), Interactable: true},
			}},
			{Role: "Cell", Frame: f(0, 360, 400, 60), Interactable: true, Children: []*Node{
				{Role: "StaticText", Label: "Bob", Frame: f(20, 375, 100, 30)},
				{Role: "Button", Label: "Delete", Identifier: "del-bob", Frame: f(320, 375, 60, 30), Interactable: true},
			}},
		},
	}}
}

var dslViewport = &Frame{X: 0, Y: 0, W: 400, H: 800}

// mustDSL parses a predicate map or fails the test.
func mustDSL(t *testing.T, m map[string]any) *dslPredicate {
	t.Helper()
	d, err := dslFromMap(m, 0)
	if err != nil {
		t.Fatalf("dslFromMap(%v): %v", m, err)
	}
	return d
}

// resolveDSL resolves a predicate map against the canned tree.
func resolveDSL(t *testing.T, m map[string]any) (*Node, error) {
	t.Helper()
	return mustDSL(t, m).resolve(dslTree(), dslViewport)
}

func TestDSLTextExact(t *testing.T) {
	n, err := resolveDSL(t, map[string]any{"text": "Alice"})
	if err != nil || n.Label != "Alice" {
		t.Fatalf("got %v, %v", n, err)
	}
	// Exact means exact: a prefix must not match.
	if _, err := resolveDSL(t, map[string]any{"text": "Alic"}); !errors.Is(err, errNoMatch) {
		t.Fatalf("prefix text: err = %v, want errNoMatch", err)
	}
}

func TestDSLTextContains(t *testing.T) {
	n, err := resolveDSL(t, map[string]any{"text_contains": "lice"})
	if err != nil || n.Label != "Alice" {
		t.Fatalf("got %v, %v", n, err)
	}
}

func TestDSLTextRegex(t *testing.T) {
	n, err := resolveDSL(t, map[string]any{"text_regex": "^Pass"})
	if err != nil || n.Label != "Password" {
		t.Fatalf("got %v, %v", n, err)
	}
	if _, err := dslFromMap(map[string]any{"text_regex": "("}, 0); err == nil {
		t.Fatal("invalid regex accepted")
	}
	long := strings.Repeat("a", dslMaxRegexLen+1)
	if _, err := dslFromMap(map[string]any{"text_regex": long}, 0); err == nil {
		t.Fatal("over-long regex accepted")
	}
}

func TestDSLTextMatchesValue(t *testing.T) {
	tree := dslTree()
	tree[0].Children[1].Value = "me@example.com"
	d := mustDSL(t, map[string]any{"text": "me@example.com"})
	n, err := d.resolve(tree, dslViewport)
	if err != nil || n.Identifier != "email-field" {
		t.Fatalf("got %v, %v", n, err)
	}
}

func TestDSLType(t *testing.T) {
	n, err := resolveDSL(t, map[string]any{"type": "SecureTextField"})
	if err != nil || n.Identifier != "pw-field" {
		t.Fatalf("got %v, %v", n, err)
	}
}

func TestDSLAccessibilityID(t *testing.T) {
	n, err := resolveDSL(t, map[string]any{"accessibility_id": "save-bot"})
	if err != nil || n.Identifier != "save-bot" {
		t.Fatalf("got %v, %v", n, err)
	}
}

func TestDSLAmbiguityListsCandidates(t *testing.T) {
	_, err := resolveDSL(t, map[string]any{"text": "Save"})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "ambiguous_match" {
		t.Fatalf("err = %v, want ambiguous_match", err)
	}
	for _, want := range []string{"matched 2 elements", "save-top", "save-bot", "[0]", "[1]", "index"} {
		if !strings.Contains(ae.Message, want) {
			t.Fatalf("ambiguity message missing %q: %s", want, ae.Message)
		}
	}
}

func TestDSLAmbiguityCapsListing(t *testing.T) {
	var nodes []*Node
	for i := 0; i < 12; i++ {
		nodes = append(nodes, &Node{Role: "Button", Label: "Go",
			Frame: &Frame{X: 0, Y: float64(i * 50), W: 100, H: 40}})
	}
	_, err := mustDSL(t, map[string]any{"text": "Go"}).resolve(nodes, dslViewport)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "ambiguous_match" {
		t.Fatalf("err = %v, want ambiguous_match", err)
	}
	if !strings.Contains(ae.Message, "and 4 more") {
		t.Fatalf("expected elision marker: %s", ae.Message)
	}
}

func TestDSLIndexDisambiguates(t *testing.T) {
	n, err := resolveDSL(t, map[string]any{"text": "Save", "index": 1.0})
	if err != nil || n.Identifier != "save-bot" {
		t.Fatalf("got %v, %v", n, err)
	}
	// Out-of-range index is a no-match (the element may still appear).
	if _, err := resolveDSL(t, map[string]any{"text": "Save", "index": 5.0}); !errors.Is(err, errNoMatch) {
		t.Fatalf("out-of-range index: err = %v, want errNoMatch", err)
	}
}

func TestDSLZeroMatches(t *testing.T) {
	if _, err := resolveDSL(t, map[string]any{"text": "Nope"}); !errors.Is(err, errNoMatch) {
		t.Fatalf("err = %v, want errNoMatch", err)
	}
}

func TestDSLBoundsHint(t *testing.T) {
	for hint, wantID := range map[string]string{
		"top_half":    "save-top",
		"bottom_half": "save-bot",
	} {
		n, err := resolveDSL(t, map[string]any{"text": "Save", "bounds_hint": hint})
		if err != nil || n.Identifier != wantID {
			t.Fatalf("%s: got %v, %v", hint, n, err)
		}
	}
	// left_half/right_half split the Email row.
	n, err := resolveDSL(t, map[string]any{"type": "StaticText", "bounds_hint": "left_half", "text": "Email"})
	if err != nil || n.Label != "Email" {
		t.Fatalf("left_half: got %v, %v", n, err)
	}
	n, err = resolveDSL(t, map[string]any{"type": "TextField", "bounds_hint": "right_half"})
	if err != nil || n.Identifier != "email-field" {
		t.Fatalf("right_half: got %v, %v", n, err)
	}
	// center: the two Cells' centres sit in the middle half; Alice's cell first with index.
	n, err = resolveDSL(t, map[string]any{"type": "Cell", "bounds_hint": "center", "index": 0.0})
	if err != nil || n.Role != "Cell" {
		t.Fatalf("center: got %v, %v", n, err)
	}
}

func TestDSLBoundsHintWithoutViewportUsesFrameUnion(t *testing.T) {
	d := mustDSL(t, map[string]any{"text": "Save", "bounds_hint": "top_half"})
	n, err := d.resolve(dslTree(), nil)
	if err != nil || n.Identifier != "save-top" {
		t.Fatalf("got %v, %v", n, err)
	}
}

func TestDSLBoundsHintSkipsFramelessNodes(t *testing.T) {
	nodes := []*Node{
		{Role: "Button", Label: "Go"}, // no frame
		{Role: "Button", Label: "Go", Frame: &Frame{X: 0, Y: 0, W: 100, H: 40}},
	}
	n, err := mustDSL(t, map[string]any{"text": "Go", "bounds_hint": "top_half"}).resolve(nodes, dslViewport)
	if err != nil || n.Frame == nil {
		t.Fatalf("got %v, %v", n, err)
	}
}

func TestDSLNearDirections(t *testing.T) {
	// The Delete button right of "Alice" (same row) — not Bob's.
	n, err := resolveDSL(t, map[string]any{"text": "Delete", "near": map[string]any{
		"predicate": map[string]any{"text": "Alice"}, "direction": "right"}})
	if err != nil || n.Identifier != "del-alice" {
		t.Fatalf("right: got %v, %v", n, err)
	}
	// The label left of the email field.
	n, err = resolveDSL(t, map[string]any{"type": "StaticText", "near": map[string]any{
		"predicate": map[string]any{"accessibility_id": "email-field"}, "direction": "left"}})
	if err != nil || n.Label != "Email" {
		t.Fatalf("left: got %v, %v", n, err)
	}
	// above/below between the two labelled rows.
	n, err = resolveDSL(t, map[string]any{"type": "StaticText", "near": map[string]any{
		"predicate": map[string]any{"text": "Email"}, "direction": "below", "max_distance": 100.0}})
	if err != nil || n.Label != "Password" {
		t.Fatalf("below: got %v, %v", n, err)
	}
	n, err = resolveDSL(t, map[string]any{"type": "StaticText", "near": map[string]any{
		"predicate": map[string]any{"text": "Password"}, "direction": "up"}})
	if err != nil || n.Label != "Email" {
		t.Fatalf("up alias: got %v, %v", n, err)
	}
}

func TestDSLNearMaxDistance(t *testing.T) {
	// Bob's Delete is ~300pt right-down of "Alice"; a tight cap excludes it
	// but keeps Alice's own Delete (~256pt away, same row).
	n, err := resolveDSL(t, map[string]any{"text": "Delete", "near": map[string]any{
		"predicate": map[string]any{"text": "Alice"}, "direction": "right", "max_distance": 280.0}})
	if err != nil || n.Identifier != "del-alice" {
		t.Fatalf("got %v, %v", n, err)
	}
	// A cap tighter than any candidate leaves nothing.
	_, err = resolveDSL(t, map[string]any{"text": "Delete", "near": map[string]any{
		"predicate": map[string]any{"text": "Alice"}, "direction": "right", "max_distance": 10.0}})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("err = %v, want errNoMatch", err)
	}
}

func TestDSLNearAmbiguousAnchorPropagates(t *testing.T) {
	// The near anchor itself matches two elements — the ambiguity error
	// must surface rather than a silent pick.
	_, err := resolveDSL(t, map[string]any{"type": "Button", "near": map[string]any{
		"predicate": map[string]any{"text": "Save"}, "direction": "below"}})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "ambiguous_match" {
		t.Fatalf("err = %v, want ambiguous_match from the anchor", err)
	}
}

func TestDSLNearMissingAnchorIsNoMatch(t *testing.T) {
	_, err := resolveDSL(t, map[string]any{"text": "Delete", "near": map[string]any{
		"predicate": map[string]any{"text": "Carol"}, "direction": "right"}})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("err = %v, want errNoMatch", err)
	}
}

func TestDSLParentOfBareResolvesDirectParent(t *testing.T) {
	n, err := resolveDSL(t, map[string]any{"parent_of": map[string]any{"text": "Alice"}})
	if err != nil || n.Role != "Cell" {
		t.Fatalf("got %v, %v", n, err)
	}
	if n.Frame == nil || n.Frame.Y != 300 {
		t.Fatalf("wrong cell: %+v", n.Frame)
	}
}

func TestDSLParentOfConstrainedMatchesAncestor(t *testing.T) {
	// The Window is the only element of type Window containing "Alice".
	n, err := resolveDSL(t, map[string]any{"type": "Window", "parent_of": map[string]any{"text": "Alice"}})
	if err != nil || n.Role != "Window" {
		t.Fatalf("got %v, %v", n, err)
	}
	// Constrained to Cell it is Alice's cell, not Bob's.
	n, err = resolveDSL(t, map[string]any{"type": "Cell", "parent_of": map[string]any{"text": "Alice"}})
	if err != nil || n.Role != "Cell" || n.Frame.Y != 300 {
		t.Fatalf("got %v, %v", n, err)
	}
}

func TestDSLParentOfAmbiguousDescendants(t *testing.T) {
	// "Delete" matches in both cells → two parents → ambiguous, listed.
	_, err := resolveDSL(t, map[string]any{"parent_of": map[string]any{"text": "Delete"}})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "ambiguous_match" {
		t.Fatalf("err = %v, want ambiguous_match", err)
	}
}

func TestDSLParentOfWithNestedIndex(t *testing.T) {
	// Index inside parent_of picks Bob's Delete → Bob's cell.
	n, err := resolveDSL(t, map[string]any{"parent_of": map[string]any{"text": "Delete", "index": 1.0}})
	if err != nil || n.Role != "Cell" || n.Frame.Y != 360 {
		t.Fatalf("got %v, %v", n, err)
	}
}

func TestDSLParentOfNoDescendant(t *testing.T) {
	_, err := resolveDSL(t, map[string]any{"parent_of": map[string]any{"text": "Carol"}})
	if !errors.Is(err, errNoMatch) {
		t.Fatalf("err = %v, want errNoMatch", err)
	}
}

func TestDSLCombinedNearAndParentOf(t *testing.T) {
	// The cell containing "Bob", found as the cell below Alice's row.
	n, err := resolveDSL(t, map[string]any{
		"parent_of": map[string]any{"text": "Bob"},
		"near": map[string]any{
			"predicate": map[string]any{"accessibility_id": "del-alice"}, "direction": "below"},
	})
	if err != nil || n.Role != "Cell" || n.Frame.Y != 360 {
		t.Fatalf("got %v, %v", n, err)
	}
}

func TestDSLParseErrors(t *testing.T) {
	cases := []map[string]any{
		{"txet": "typo"},                    // unknown field
		{"text": "a", "text_contains": "b"}, // exclusive text forms
		{"index": 1.0},                      // index alone selects nothing
		{},                                  // empty predicate
		{"text": 5.0},                       // non-string
		{"index": -1.0},                     // negative index
		{"index": 1.5},                      // fractional index
		{"bounds_hint": "middle"},           // bad hint
		{"near": "left"},                    // near not an object
		{"near": map[string]any{"direction": "left"}},                                                                 // near without predicate
		{"near": map[string]any{"predicate": map[string]any{"text": "x"}, "direction": "sideways"}},                   // bad direction
		{"near": map[string]any{"predicate": map[string]any{"text": "x"}, "direction": "left", "max_distance": -5.0}}, // bad distance
		{"near": map[string]any{"predicate": map[string]any{"text": "x"}, "direction": "left", "max_distanse": 5.0}},  // typo field
		{"parent_of": "Alice"}, // parent_of not an object
	}
	for _, m := range cases {
		if _, err := dslFromMap(m, 0); err == nil {
			t.Errorf("dslFromMap(%v): want error, got nil", m)
		} else if ae, ok := err.(*Error); !ok || ae.Code != "bad_request" {
			t.Errorf("dslFromMap(%v): err = %v, want bad_request", m, err)
		}
	}
}

func TestDSLNestingDepthLimit(t *testing.T) {
	m := map[string]any{"text": "x"}
	for i := 0; i < dslMaxDepth+1; i++ {
		m = map[string]any{"parent_of": m}
	}
	if _, err := dslFromMap(m, 0); err == nil {
		t.Fatal("over-deep predicate accepted")
	}
}

func TestMatcherFromPayloadRejectsMixedForms(t *testing.T) {
	_, err := matcherFromPayload(map[string]any{
		"label":     "Save",
		"predicate": map[string]any{"text": "Save"},
	})
	if ae, ok := err.(*Error); !ok || ae.Code != "bad_request" {
		t.Fatalf("err = %v, want bad_request", err)
	}
}

func TestMatcherFromPayloadIgnoresDefaultFlatFields(t *testing.T) {
	m, err := matcherFromPayload(map[string]any{
		"label":     "",
		"exact":     false,
		"in_frame":  nil,
		"predicate": map[string]any{"text": "Alice"},
	})
	if err != nil {
		t.Fatalf("default-valued flat fields rejected: %v", err)
	}
	if n, err := m.resolve(dslTree(), dslViewport); err != nil || n.Label != "Alice" {
		t.Fatalf("got %v, %v", n, err)
	}
	if _, err := matcherFromPayload(map[string]any{
		"exact":     true,
		"predicate": map[string]any{"text": "Alice"},
	}); err == nil {
		t.Fatal("exact=true combined with predicate accepted")
	}
	m, err = matcherFromPayload(map[string]any{"label": "Alice", "predicate": nil})
	if err != nil {
		t.Fatalf("nil predicate rejected: %v", err)
	}
	if n, err := m.resolve(dslTree(), dslViewport); err != nil || n.Label != "Alice" {
		t.Fatalf("got %v, %v", n, err)
	}
}

func TestMatcherFromPayloadFlatStillWorks(t *testing.T) {
	m, err := matcherFromPayload(map[string]any{"label": "Alice"})
	if err != nil {
		t.Fatalf("flat matcher: %v", err)
	}
	n, err := m.resolve(dslTree(), dslViewport)
	if err != nil || n.Label != "Alice" {
		t.Fatalf("got %v, %v", n, err)
	}
	// The flat matcher keeps its lenient best-match ranking: two "Save"
	// buttons resolve (no ambiguity error) for backward compatibility.
	m, err = matcherFromPayload(map[string]any{"label": "Save"})
	if err != nil {
		t.Fatalf("flat matcher: %v", err)
	}
	if n, err := m.resolve(dslTree(), dslViewport); err != nil || n.Label != "Save" {
		t.Fatalf("got %v, %v", n, err)
	}
}

// End-to-end through the composite actions and canned describe-ui JSON.

// dslTreeJSON is a raw describe-ui document matching a subset of dslTree.
const dslTreeJSON = `[{"role":"Window","frame":{"x":0,"y":0,"width":400,"height":800},"children":[
  {"role":"Button","AXLabel":"Save","AXUniqueId":"save-top","frame":{"x":20,"y":40,"width":100,"height":40}},
  {"role":"Button","AXLabel":"Save","AXUniqueId":"save-bot","frame":{"x":20,"y":700,"width":100,"height":40}},
  {"role":"StaticText","AXLabel":"Email","frame":{"x":20,"y":100,"width":80,"height":20}},
  {"role":"TextField","AXUniqueId":"email-field","frame":{"x":120,"y":100,"width":260,"height":30}}
]}]`

func TestTapElementWithDSLPredicate(t *testing.T) {
	b, r := compositeBackend(seqStep{stdout: dslTreeJSON})
	res, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"predicate": map[string]any{
			"text": "Save", "bounds_hint": "bottom_half"}}))
	if err != nil {
		t.Fatalf("tap_element: %v", err)
	}
	el, ok := res["element"].(*Node)
	if !ok || el.Identifier != "save-bot" {
		t.Fatalf("bad element: %+v", res["element"])
	}
	// save-bot: (20,700 100x40) → centre (70, 720).
	if tap := lastCall(r, "tap"); !hasArgs(tap, []string{"tap", "-x", "70", "-y", "720"}) {
		t.Fatalf("tap args = %v", tap)
	}
}

func TestTapElementDSLAmbiguityFailsFast(t *testing.T) {
	b, _ := seqBackend(seqStep{stdout: dslTreeJSON})
	_, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"predicate": map[string]any{"text": "Save"}}))
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "ambiguous_match" {
		t.Fatalf("err = %v, want ambiguous_match", err)
	}
}

func TestTapElementDSLNear(t *testing.T) {
	b, _ := compositeBackend(seqStep{stdout: dslTreeJSON})
	res, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"predicate": map[string]any{
			"type": "TextField",
			"near": map[string]any{"predicate": map[string]any{"text": "Email"},
				"direction": "right"}}}))
	if err != nil {
		t.Fatalf("tap_element: %v", err)
	}
	if el := res["element"].(*Node); el.Identifier != "email-field" {
		t.Fatalf("bad element: %+v", el)
	}
}

func TestWaitForElementDSLTimeoutOnNoMatch(t *testing.T) {
	b, _ := seqBackend(seqStep{stdout: dslTreeJSON})
	_, err := handleWaitForElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"predicate": map[string]any{"text": "Missing"}}))
	if e, ok := err.(*Error); !ok || e.Code != "timeout" {
		t.Fatalf("err = %v, want timeout", err)
	}
	if !strings.Contains(err.Error(), `text="Missing"`) {
		t.Fatalf("timeout should echo the predicate: %v", err)
	}
}

func TestWaitForElementDSLAbsentWithAmbiguity(t *testing.T) {
	// absent:true with a predicate still matching several elements means
	// "still present" — the wait times out instead of erroring ambiguous.
	b, _ := seqBackend(seqStep{stdout: dslTreeJSON})
	_, err := handleWaitForElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"absent": true,
			"predicate": map[string]any{"text": "Save"}}))
	if e, ok := err.(*Error); !ok || e.Code != "timeout" {
		t.Fatalf("err = %v, want timeout", err)
	}
}
