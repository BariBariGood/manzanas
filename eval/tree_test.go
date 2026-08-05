package eval

import "testing"

func sampleTree() any {
	return map[string]any{
		"role": "Window",
		"children": []any{
			map[string]any{"role": "Button", "label": "General", "value": ""},
			map[string]any{
				"role": "Group",
				"children": []any{
					map[string]any{"role": "StaticText", "label": "Battery", "value": "100%"},
				},
			},
		},
	}
}

func TestFindElement(t *testing.T) {
	tree := sampleTree()
	cases := []struct {
		name string
		q    ElementQuery
		want bool
	}{
		{"by label", ElementQuery{Label: "General"}, true},
		{"label substring", ElementQuery{Label: "Gen"}, true},
		{"nested", ElementQuery{Label: "Battery"}, true},
		{"role+label", ElementQuery{Role: "StaticText", Label: "Battery"}, true},
		{"role mismatch", ElementQuery{Role: "Button", Label: "Battery"}, false},
		{"by value", ElementQuery{Value: "100%"}, true},
		{"absent", ElementQuery{Label: "Nope"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := findElement(tree, &tc.q); got != tc.want {
				t.Errorf("findElement(%s) = %v, want %v", tc.q.String(), got, tc.want)
			}
		})
	}
}

func TestElementQueryString(t *testing.T) {
	q := ElementQuery{Role: "Button", Label: "General"}
	if got := q.String(); got != "role=Button label~General" {
		t.Errorf("String() = %q", got)
	}
}
