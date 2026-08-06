package actions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

func TestCompactTree(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []*Node
	}{
		{
			name: "collapses noise wrappers",
			raw: `[{"type":"Application","AXLabel":"","frame":{"x":0,"y":0,"width":393,"height":852},
			      "children":[{"type":"Other","children":[
			        {"type":"Button","AXLabel":"Wi-Fi","frame":{"x":16,"y":100,"width":361,"height":44}}]}]}]`,
			want: []*Node{{
				Role:         "Button",
				Label:        "Wi-Fi",
				Frame:        &Frame{X: 16, Y: 100, W: 361, H: 44},
				Interactable: true,
			}},
		},
		{
			name: "keeps labelled containers and nests children",
			raw: `[{"type":"Other","AXLabel":"Settings","children":[
			        {"type":"StaticText","AXLabel":"General"}]}]`,
			want: []*Node{{
				Role:  "Other",
				Label: "Settings",
				Children: []*Node{{
					Role:  "StaticText",
					Label: "General",
				}},
			}},
		},
		{
			name: "parses AXFrame string form and disabled flag",
			raw:  `[{"AXType":"AXButton","AXLabel":"Done","AXFrame":"{{10, 20}, {80, 30}}","enabled":false}]`,
			want: []*Node{{
				Role:         "Button",
				Label:        "Done",
				Frame:        &Frame{X: 10, Y: 20, W: 80, H: 30},
				Interactable: true,
				Disabled:     true,
			}},
		},
		{
			name: "keeps text fields with value, placeholder and identifier",
			raw: `[{"type":"TextField","AXValue":"alice","AXPlaceholderValue":"Search",
			       "AXUniqueId":"search-field","frame":{"x":0,"y":0,"width":100,"height":40}}]`,
			want: []*Node{{
				Role:         "TextField",
				Value:        "alice",
				Placeholder:  "Search",
				Identifier:   "search-field",
				Frame:        &Frame{X: 0, Y: 0, W: 100, H: 40},
				Interactable: true,
			}},
		},
		{
			name: "drops empty wrappers entirely",
			raw:  `[{"type":"Other","children":[{"type":"Group"}]}]`,
			want: nil,
		},
		{
			name: "accepts a single object root",
			raw:  `{"type":"Cell","AXLabel":"Airplane Mode"}`,
			want: []*Node{{Role: "Cell", Label: "Airplane Mode", Interactable: true}},
		},
		{
			// UIKit includes this wrapper nondeterministically; keeping it
			// would make the tree hash flap on an unchanged screen.
			name: "collapses wrappers whose label merely restates their id",
			raw: `[{"type":"Group","AXLabel":"Toolbar","AXUniqueId":"Toolbar","children":[
			        {"type":"SearchField","AXPlaceholderValue":"Search"}]}]`,
			want: []*Node{{
				Role:         "SearchField",
				Placeholder:  "Search",
				Interactable: true,
			}},
		},
		{
			name: "keeps id-only container wrappers for id-based lookups",
			raw:  `[{"type":"Other","AXUniqueId":"ContentView","children":[{"type":"Button","AXLabel":"OK"}]}]`,
			want: []*Node{{
				Role:       "Other",
				Identifier: "ContentView",
				Children:   []*Node{{Role: "Button", Label: "OK", Interactable: true}},
			}},
		},
		{
			name: "keeps childless identifier-only elements",
			raw:  `[{"type":"Other","AXUniqueId":"login-form","frame":{"x":0,"y":0,"width":100,"height":40}}]`,
			want: []*Node{{
				Role:       "Other",
				Identifier: "login-form",
				Frame:      &Frame{X: 0, Y: 0, W: 100, H: 40},
			}},
		},
		{
			name: "treats (null) labels as empty",
			raw:  `[{"type":"Button","AXLabel":"(null)","AXValue":"on"}]`,
			want: []*Node{{Role: "Button", Value: "on", Interactable: true}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompactTree([]byte(tc.raw))
			if err != nil {
				t.Fatalf("CompactTree: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %s\nwant %s", mustJSON(t, got), mustJSON(t, tc.want))
			}
		})
	}
}

func TestCompactTreeRejectsNonJSON(t *testing.T) {
	if _, err := CompactTree([]byte("No translation object returned")); err == nil {
		t.Fatal("want error for non-JSON output")
	}
}

func TestTreeHashChangesWithTree(t *testing.T) {
	a, _ := CompactTree([]byte(`[{"type":"Button","AXLabel":"On"}]`))
	b, _ := CompactTree([]byte(`[{"type":"Button","AXLabel":"Off"}]`))
	if TreeHash(a) == "" || TreeHash(a) == TreeHash(b) {
		t.Fatalf("hashes should be non-empty and differ: %q vs %q", TreeHash(a), TreeHash(b))
	}
	again, _ := CompactTree([]byte(`[{"type":"Button","AXLabel":"On"}]`))
	if TreeHash(a) != TreeHash(again) {
		t.Fatal("hash is not stable for identical trees")
	}
}

func TestFrameCenter(t *testing.T) {
	x, y := Frame{X: 10, Y: 20, W: 100, H: 40}.Center()
	if x != 60 || y != 40 {
		t.Fatalf("center = (%v,%v), want (60,40)", x, y)
	}
}

func TestObserveRetriesTransientA11yError(t *testing.T) {
	observeBackoff = time.Millisecond
	f := newFakeRunner()
	f.failFirst["describe-ui"] = 2
	f.stdout["describe-ui"] = `[{"type":"Button","AXLabel":"Wi-Fi","frame":{"x":0,"y":0,"width":10,"height":10}}]`

	res, err := dispatch(t, testBackend(f), "observe", nil)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if n := len(f.argvs()); n != 3 {
		t.Fatalf("describe-ui called %d times, want 3 (2 retries)", n)
	}
	if res.Result["hash"] == "" {
		t.Fatal("missing tree hash")
	}
	nodes, ok := res.Result["tree"].([]*Node)
	if !ok || len(nodes) != 1 || nodes[0].Label != "Wi-Fi" {
		t.Fatalf("unexpected tree: %+v", res.Result["tree"])
	}
}

// A skeleton (empty-compacting) tree right after launch is re-polled a
// bounded number of times, then returned as a legitimately empty screen.
func TestObserveRetriesEmptyTreeThenReturnsIt(t *testing.T) {
	observeBackoff = time.Millisecond
	f := newFakeRunner()
	f.stdout["describe-ui"] = `[{"type":"Application","children":[{"type":"Group"}]}]`
	res, err := dispatch(t, testBackend(f), "observe", nil)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	// The wire shape must be an empty list, not JSON null.
	if b, err := json.Marshal(res.Result["tree"]); err != nil || string(b) != "[]" {
		t.Fatalf("tree marshals to %s (%v), want []", b, err)
	}
	if n := len(f.argvs()); n != emptyTreeRetries+1 {
		t.Fatalf("describe-ui called %d times, want %d", n, emptyTreeRetries+1)
	}
	// The distinct detail marks the empty snapshot as retryable for callers.
	if res.Result["detail"] != "empty_tree" {
		t.Fatalf("detail = %v, want empty_tree", res.Result["detail"])
	}
}

func TestObserveNonEmptyTreeHasNoDetail(t *testing.T) {
	f := newFakeRunner()
	f.stdout["describe-ui"] = `[{"type":"Button","AXLabel":"Wi-Fi","frame":{"x":0,"y":0,"width":10,"height":10}}]`
	res, err := dispatch(t, testBackend(f), "observe", nil)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if _, ok := res.Result["detail"]; ok {
		t.Fatalf("detail = %v, want absent", res.Result["detail"])
	}
}

// wait_for_element must treat an empty snapshot as "not yet" and keep
// polling until the tree comes back — never as a completed absence.
func TestWaitForElementPollsThroughEmptyTrees(t *testing.T) {
	b, r := seqBackend(
		seqStep{stdout: `[]`},
		seqStep{stdout: `[{"type":"Application","children":[{"type":"Group"}]}]`},
		seqStep{stdout: treeJSON("Settings", "General")},
	)
	res, err := handleWaitForElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "General"}))
	if err != nil {
		t.Fatalf("wait_for_element: %v", err)
	}
	if el, ok := res["element"].(*Node); !ok || el.Label != "General" {
		t.Fatalf("bad element: %+v", res["element"])
	}
	r.mu.Lock()
	calls := r.calls
	r.mu.Unlock()
	if calls < 3 {
		t.Fatalf("describe-ui called %d times, want >= 3 (polled through empty trees)", calls)
	}
}

// wait_for_element with absent:true must NOT read an empty snapshot as
// the element being gone; the tree has to come back non-empty without
// the element first.
func TestWaitForElementAbsentIgnoresEmptyTrees(t *testing.T) {
	b, _ := seqBackend(
		seqStep{stdout: treeJSON("Settings", "General")},
		seqStep{stdout: `[]`},
		seqStep{stdout: `[]`},
		seqStep{stdout: treeJSON("Settings")},
	)
	res, err := handleWaitForElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "General", "absent": true}))
	if err != nil {
		t.Fatalf("wait_for_element absent: %v", err)
	}
	if polls, _ := res["polls"].(int); polls < 4 {
		t.Fatalf("polls = %v, want >= 4 (empty trees must not count as absence)", res["polls"])
	}
}

func TestObserveGivesUpAfterRetries(t *testing.T) {
	observeBackoff = time.Millisecond
	f := newFakeRunner()
	f.failFirst["describe-ui"] = observeRetries + 1
	_, err := dispatch(t, testBackend(f), "observe", nil)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != proto.ErrUnavailable {
		t.Fatalf("want unavailable error after exhausting retries, got %v", err)
	}
	if n := len(f.argvs()); n != observeRetries {
		t.Fatalf("describe-ui called %d times, want %d", n, observeRetries)
	}
}

func TestObserveNonTransientErrorFailsFast(t *testing.T) {
	f := newFakeRunner()
	f.errs["describe-ui"] = "unknown simulator"
	if _, err := dispatch(t, testBackend(f), "observe", nil); err == nil {
		t.Fatal("want error")
	}
	if n := len(f.argvs()); n != 1 {
		t.Fatalf("describe-ui called %d times, want 1 (no retry)", n)
	}
}

func TestObserveContextCancellation(t *testing.T) {
	observeBackoff = time.Hour
	defer func() { observeBackoff = time.Millisecond }()
	f := newFakeRunner()
	f.failFirst["describe-ui"] = observeRetries
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	b := testBackend(f)
	if _, err := b.Dispatch(ctx, udid, proto.ActionRequest{Kind: "observe"}); err == nil {
		t.Fatal("want error when context expires during backoff")
	}
}

func TestRawViewport(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want *Frame
	}{
		{name: "root application frame",
			raw:  `[{"role":"Application","frame":{"x":0,"y":0,"width":400,"height":874},"children":[]}]`,
			want: &Frame{W: 400, H: 874}},
		{name: "over-tall scroll content container below the screen wrapper",
			raw: `[{"role":"AXApplication","children":[{"role":"AXGroup","frame":{"x":0,"y":0,"width":402,"height":874},"children":[` +
				`{"role":"AXScrollArea","frame":{"x":0,"y":0,"width":402,"height":3000},"children":[]}]}]}]`,
			want: &Frame{W: 402, H: 874}},
		{name: "AXChildren-shaped doc, screen frame on wrapper group",
			raw:  `[{"type":"Application","AXChildren":[{"type":"Group","frame":{"x":0,"y":0,"width":402,"height":874},"AXChildren":[]}]}]`,
			want: &Frame{W: 402, H: 874}},
		{name: "frameless root, screen frame on wrapper group",
			raw:  `[{"role":"AXApplication","children":[{"role":"AXGroup","frame":{"x":0,"y":0,"width":402,"height":874},"children":[]}]}]`,
			want: &Frame{W: 402, H: 874}},
		{name: "non-origin root is not a viewport",
			raw: `[{"role":"Group","frame":{"x":10,"y":50,"width":200,"height":100}}]`},
		{name: "missing frame", raw: `[{"role":"Application"}]`},
		{name: "empty document", raw: `[]`},
		{name: "not json", raw: `nope`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rawViewport([]byte(tt.raw))
			if (got == nil) != (tt.want == nil) {
				t.Fatalf("rawViewport = %+v, want %+v", got, tt.want)
			}
			if got != nil && (got.W != tt.want.W || got.H != tt.want.H) {
				t.Fatalf("rawViewport = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRawViewportRealDescribeUI(t *testing.T) {
	raw, err := os.ReadFile("testdata/describe-ui-settings.json")
	if err != nil {
		t.Fatal(err)
	}
	vp := rawViewport(raw)
	if vp == nil || vp.W != 402 || vp.H != 874 {
		t.Fatalf("rawViewport = %+v, want 402x874", vp)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
