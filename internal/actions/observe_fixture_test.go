package actions

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestCompactRealDescribeUI runs the compaction over an excerpt of real
// `axe describe-ui` output captured from Settings on an iPhone 17 Pro
// simulator (iOS 26.5).
func TestCompactRealDescribeUI(t *testing.T) {
	raw, err := os.ReadFile("testdata/describe-ui-settings.json")
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := CompactTree(raw)
	if err != nil {
		t.Fatalf("CompactTree: %v", err)
	}
	compact, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(compact) >= len(raw)/2 {
		t.Fatalf("compaction saved too little: %d -> %d bytes", len(raw), len(compact))
	}

	flat := flatten(nodes)
	appleAccount := findByLabelPrefix(flat, "Apple Account")
	if appleAccount == nil {
		t.Fatalf("Apple Account button missing from tree: %s", compact)
	}
	if appleAccount.Role != "Button" || !appleAccount.Interactable {
		t.Fatalf("Apple Account node = %+v, want interactable Button", appleAccount)
	}
	if x, y := appleAccount.Frame.Center(); x != 201 || y != 213.16666666666666 {
		t.Fatalf("frame center = (%v,%v)", x, y)
	}
	// The button's AXLabel already concatenates its child texts, so those
	// child StaticText/Image nodes must be pruned.
	if len(appleAccount.Children) != 0 {
		t.Fatalf("decoration not pruned: %+v", appleAccount.Children)
	}

	var field *Node
	for _, n := range flat {
		if n.Role == "TextField" {
			field = n
		}
	}
	if field == nil || field.Value != "Search" || !field.Interactable {
		t.Fatalf("search field not preserved: %+v", field)
	}
	// A nested real control (the dictation button) survives pruning, and
	// the duplicate copy UIKit exposes is deduped.
	var dictate int
	for _, n := range flatten(field.Children) {
		if n.Label == "Dictate" {
			dictate++
		}
	}
	if dictate != 1 {
		t.Fatalf("Dictate button appears %d times, want 1: %+v", dictate, field.Children)
	}
	if TreeHash(nodes) == "" {
		t.Fatal("empty hash")
	}
}

func flatten(nodes []*Node) []*Node {
	var out []*Node
	for _, n := range nodes {
		out = append(out, n)
		out = append(out, flatten(n.Children)...)
	}
	return out
}

func findByLabelPrefix(nodes []*Node, prefix string) *Node {
	if prefix == "" {
		return nil
	}
	for _, n := range nodes {
		if strings.HasPrefix(n.Label, prefix) {
			return n
		}
	}
	return nil
}
