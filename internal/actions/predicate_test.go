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

func TestPredicateString(t *testing.T) {
	pr := predicate{Label: "General", Role: "Cell", Exact: true, InFrame: &Frame{X: 0, Y: 0, W: 100, H: 50}}
	got := pr.String()
	want := `label="General" role="Cell" exact in_frame=(0,0 100x50)`
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
