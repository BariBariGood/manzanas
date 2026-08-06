package actions

import "testing"

func testTree() []*Node {
	return []*Node{
		{
			Role: "Window",
			Children: []*Node{
				{Role: "Cell", Label: "General", Identifier: "general-cell",
					Frame: &Frame{X: 0, Y: 100, W: 400, H: 44}, Interactable: true},
				{Role: "Cell", Label: "General Motors",
					Frame: &Frame{X: 0, Y: 144, W: 400, H: 44}, Interactable: true},
				{Role: "TextField", Placeholder: "Search", Value: "hello world",
					Frame: &Frame{X: 0, Y: 600, W: 400, H: 36}},
			},
		},
	}
}

func TestPredicateMatching(t *testing.T) {
	tree := testTree()
	tests := []struct {
		name      string
		payload   map[string]any
		wantLabel string // label of the expected match; "" means no match
		wantErr   bool
	}{
		{name: "label substring", payload: map[string]any{"label": "Gener"}, wantLabel: "General"},
		{name: "label exact", payload: map[string]any{"label": "General Motors", "exact": true}, wantLabel: "General Motors"},
		{name: "label exact no match", payload: map[string]any{"label": "Gener", "exact": true}, wantLabel: "<none>"},
		{name: "role exact only", payload: map[string]any{"role": "TextField"}, wantLabel: ""},
		{name: "role partial never matches", payload: map[string]any{"role": "Text"}, wantErr: false, wantLabel: "<none>"},
		{name: "id", payload: map[string]any{"id": "general-cell"}, wantLabel: "General"},
		{name: "value substring", payload: map[string]any{"value": "world"}, wantLabel: ""},
		{name: "label+role combined", payload: map[string]any{"label": "General", "role": "Cell"}, wantLabel: "General"},
		{name: "in_frame filters", payload: map[string]any{"label": "General", "in_frame": map[string]any{"x": 0.0, "y": 130.0, "w": 400.0, "h": 100.0}}, wantLabel: "General Motors"},
		{name: "in_frame excludes all", payload: map[string]any{"label": "General", "in_frame": map[string]any{"x": 0.0, "y": 700.0, "w": 400.0, "h": 100.0}}, wantLabel: "<none>"},
		{name: "empty predicate rejected", payload: map[string]any{}, wantErr: true},
		{name: "bad in_frame rejected", payload: map[string]any{"label": "x", "in_frame": "nope"}, wantErr: true},
		{name: "in_frame zero size rejected", payload: map[string]any{"label": "x", "in_frame": map[string]any{"x": 0.0, "y": 0.0, "w": 0.0, "h": 10.0}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr, err := predicateFromPayload(tt.payload)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", pr)
				}
				return
			}
			if err != nil {
				t.Fatalf("predicateFromPayload: %v", err)
			}
			hit := pr.find(tree)
			switch {
			case tt.wantLabel == "<none>":
				if hit != nil {
					t.Fatalf("expected no match, got %+v", hit)
				}
			case tt.wantLabel == "":
				// Matching a node whose label is empty (e.g. the TextField):
				// require a hit with an empty label.
				if hit == nil || hit.Label != "" {
					t.Fatalf("expected empty-label match, got %+v", hit)
				}
			default:
				if hit == nil || hit.Label != tt.wantLabel {
					t.Fatalf("want label %q, got %+v", tt.wantLabel, hit)
				}
			}
		})
	}
}

func TestPredicatePlaceholder(t *testing.T) {
	tree := []*Node{
		{Role: "TextField", Placeholder: "you@kitchen.com",
			Frame: &Frame{X: 20, Y: 300, W: 350, H: 44}, Interactable: true},
		{Role: "TextField", Value: "Paste a recipe or video link",
			Frame: &Frame{X: 20, Y: 360, W: 350, H: 44}, Interactable: true},
	}
	tests := []struct {
		name    string
		payload map[string]any
		wantY   float64
		wantErr bool
	}{
		{name: "placeholder alone is a valid predicate", payload: map[string]any{"placeholder": "you@kitchen.com"}, wantY: 300},
		{name: "placeholder substring", payload: map[string]any{"placeholder": "kitchen"}, wantY: 300},
		{name: "placeholder matches value (RN placeholder-as-value)", payload: map[string]any{"placeholder": "Paste a recipe"}, wantY: 360},
		{name: "placeholder exact against value", payload: map[string]any{"placeholder": "Paste a recipe or video link", "exact": true}, wantY: 360},
		{name: "placeholder exact miss", payload: map[string]any{"placeholder": "Paste a recipe", "exact": true}, wantY: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr, err := predicateFromPayload(tt.payload)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", pr)
				}
				return
			}
			if err != nil {
				t.Fatalf("predicateFromPayload: %v", err)
			}
			hit := pr.findBest(tree, nil)
			if tt.wantY < 0 {
				if hit != nil {
					t.Fatalf("expected no match, got %+v", hit)
				}
				return
			}
			if hit == nil || hit.Frame.Y != tt.wantY {
				t.Fatalf("want match at y=%v, got %+v", tt.wantY, hit)
			}
		})
	}
}

func TestPredicateMatchOption(t *testing.T) {
	tree := testTree()
	pr, err := predicateFromPayload(map[string]any{"label": "General", "match": "exact"})
	if err != nil {
		t.Fatalf("predicateFromPayload: %v", err)
	}
	if !pr.Exact {
		t.Fatal(`match:"exact" should set Exact`)
	}
	if hit := pr.findBest(tree, nil); hit == nil || hit.Label != "General" {
		t.Fatalf("exact match hit = %+v, want General", hit)
	}
	if _, err := predicateFromPayload(map[string]any{"label": "x", "match": "fuzzy"}); err == nil {
		t.Fatal("invalid match value should be rejected")
	}
	if pr, err := predicateFromPayload(map[string]any{"label": "x", "match": "substring"}); err != nil || pr.Exact {
		t.Fatalf("match:substring: err=%v exact=%v", err, pr.Exact)
	}
}

func TestFindBestRanking(t *testing.T) {
	// A screen where a naive first-DFS-substring match picks the wrong
	// element: description copy mentioning "Estimate", a Group whose
	// label concatenates its children, and the actual Estimate button.
	tree := []*Node{
		{Role: "StaticText", Label: "Estimates within ±15% of the total",
			Frame: &Frame{X: 0, Y: 100, W: 400, H: 60}},
		{
			Role: "Group", Label: "Estimate your order Estimate",
			Frame: &Frame{X: 0, Y: 200, W: 400, H: 120},
			Children: []*Node{
				{Role: "StaticText", Label: "Estimate your order",
					Frame: &Frame{X: 0, Y: 200, W: 400, H: 40}},
				{Role: "Button", Label: "Estimate", Interactable: true,
					Frame: &Frame{X: 150, Y: 260, W: 100, H: 44}},
			},
		},
	}
	pr, err := predicateFromPayload(map[string]any{"label": "Estimate"})
	if err != nil {
		t.Fatalf("predicateFromPayload: %v", err)
	}
	hit := pr.findBest(tree, nil)
	if hit == nil || hit.Role != "Button" {
		t.Fatalf("findBest = %+v, want the Estimate button", hit)
	}
	// Sanity: naive first-match would have picked the description text.
	if first := pr.find(tree); first == nil || first.Role != "StaticText" {
		t.Fatalf("find (DFS) = %+v, expected the description text (test setup)", first)
	}
}

func TestFindBestPrefersExactOverInteractable(t *testing.T) {
	// "Finish" must match the Finish CTA exactly, not the "Finishing
	// touches" step chip that happens to be interactable and smaller.
	tree := []*Node{
		{Role: "Button", Label: "Finishing touches", Interactable: true,
			Frame: &Frame{X: 0, Y: 100, W: 80, H: 30}},
		{Role: "Button", Label: "Finish", Interactable: true,
			Frame: &Frame{X: 0, Y: 700, W: 400, H: 52}},
	}
	pr, err := predicateFromPayload(map[string]any{"label": "Finish"})
	if err != nil {
		t.Fatalf("predicateFromPayload: %v", err)
	}
	if hit := pr.findBest(tree, nil); hit == nil || hit.Label != "Finish" {
		t.Fatalf("findBest = %+v, want the exact Finish button", hit)
	}
}

func TestFindBestPrefersSmallestAndFramed(t *testing.T) {
	tree := []*Node{
		{Role: "Button", Label: "Save", Interactable: true}, // no frame: untappable
		{Role: "Button", Label: "Save", Interactable: true,
			Frame: &Frame{X: 0, Y: 0, W: 400, H: 400}},
		{Role: "Button", Label: "Save", Interactable: true,
			Frame: &Frame{X: 10, Y: 10, W: 100, H: 44}},
	}
	pr, err := predicateFromPayload(map[string]any{"label": "Save"})
	if err != nil {
		t.Fatalf("predicateFromPayload: %v", err)
	}
	hit := pr.findBest(tree, nil)
	if hit == nil || hit.Frame == nil || hit.Frame.W != 100 {
		t.Fatalf("findBest = %+v, want the smallest framed Save", hit)
	}
}

func TestPredicateString(t *testing.T) {
	pr := predicate{Label: "General", Role: "Cell", Exact: true, InFrame: &Frame{X: 0, Y: 0, W: 100, H: 50}}
	got := pr.String()
	want := `label="General" role="Cell" exact in_frame=(0,0 100x50)`
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestPredicateRejectsNonStringMatch(t *testing.T) {
	if _, err := predicateFromPayload(map[string]any{"label": "x", "match": 3}); err == nil {
		t.Fatal("non-string match must be bad_request, got nil error")
	}
}

func TestFindBestFramelessNeverOutranksFramed(t *testing.T) {
	viewport := &Frame{W: 400, H: 874}
	tree := []*Node{
		{Role: "Button", Label: "Continue", Interactable: true}, // no frame: untappable
		{Role: "StaticText", Label: "Continue and finish",
			Frame: &Frame{X: 20, Y: 1200, W: 100, H: 44}}, // off-viewport but framed
	}
	pr, err := predicateFromPayload(map[string]any{"label": "Continue"})
	if err != nil {
		t.Fatal(err)
	}
	hit := pr.findBest(tree, viewport)
	if hit == nil || hit.Frame == nil {
		t.Fatalf("findBest = %+v, want the framed candidate", hit)
	}
}

func TestFindBestPrefersOnViewport(t *testing.T) {
	viewport := &Frame{W: 400, H: 874}
	tree := []*Node{
		{Role: "Button", Label: "Continue", Interactable: true,
			Frame: &Frame{X: 20, Y: 1200, W: 100, H: 44}}, // scrolled-away duplicate
		{Role: "Button", Label: "Continue", Interactable: true,
			Frame: &Frame{X: 20, Y: 700, W: 360, H: 44}},
	}
	pr, err := predicateFromPayload(map[string]any{"label": "Continue"})
	if err != nil {
		t.Fatal(err)
	}
	hit := pr.findBest(tree, viewport)
	if hit == nil || hit.Frame.Y != 700 {
		t.Fatalf("findBest = %+v, want the on-viewport button at y=700", hit)
	}
	// Without a viewport the smaller off-screen frame would win; with one,
	// the visible match outranks a better-scoring off-screen duplicate.
	if hit := pr.findBest(tree, nil); hit.Frame.Y != 1200 {
		t.Fatalf("nil-viewport findBest = %+v, want smallest frame at y=1200", hit)
	}
}
