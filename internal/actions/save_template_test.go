package actions

import "testing"

func TestSaveTemplateSpecFromPayload(t *testing.T) {
	rect := func(x, y, w, h float64) map[string]any {
		return map[string]any{"x": x, "y": y, "w": w, "h": h}
	}
	spec, err := saveTemplateSpecFromPayload(map[string]any{
		"name": "login", "rect": rect(1, 2, 3, 4),
	})
	if err != nil {
		t.Fatal(err)
	}
	if spec.bundleID != defaultTemplateBundle || spec.rect.W != 3 {
		t.Fatalf("bad spec: %+v", spec)
	}

	bad := []map[string]any{
		{"rect": rect(0, 0, 1, 1)},                // no name
		{"name": "a@b", "rect": rect(0, 0, 1, 1)}, // bad name
		{"name": "n"}, // no rect
		{"name": "n", "rect": rect(0.5, 0, 1, 1)},                   // non-integer
		{"name": "n", "rect": rect(-1, 0, 1, 1)},                    // negative
		{"name": "n", "rect": rect(0, 0, 0, 1)},                     // zero w
		{"name": "n", "rect": rect(5e18, 0, 5e18, 1)},               // overflow-sized
		{"name": "n", "rect": rect(0, 0, 1, 1), "bundle_id": ""},    // empty bundle
		{"name": "n", "rect": rect(0, 0, 1, 1), "bundle_id": "a/b"}, // bad bundle
	}
	for i, p := range bad {
		if _, err := saveTemplateSpecFromPayload(p); err == nil {
			t.Errorf("case %d: want error for %+v", i, p)
		}
	}
}
