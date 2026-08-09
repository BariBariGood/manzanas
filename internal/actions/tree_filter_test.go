package actions

import (
	"encoding/json"
	"strings"
	"testing"
)

func filterFixture() []*Node {
	save := anode("Button", "Save", 20, 100, 100, 44, true)
	title := anode("StaticText", "Details", 20, 40, 200, 30, false)
	field := anode("TextField", "", 20, 160, 300, 44, true)
	field.Placeholder = "Email"
	group := anode("Other", "", 0, 0, 393, 400, false, title, save, field)
	group.Identifier = "form"
	keyboard := anode("Keyboard", "", 0, 650, 393, 200, false,
		anode("Key", "q", 10, 700, 32, 42, true))
	scrollbar := anode("Slider", "", 389, 100, 3, 400, true)
	return []*Node{group, keyboard, scrollbar}
}

func TestFilterInteractiveOnly(t *testing.T) {
	out := filterNodes(filterFixture(), treeFilter{interactiveOnly: true})
	if countNodes(out) >= countNodes(filterFixture()) {
		t.Fatalf("interactive filter did not shrink the tree")
	}
	var labels []string
	walkNodes(out, func(n *Node) { labels = append(labels, n.Role+":"+n.Label) })
	joined := strings.Join(labels, ",")
	if !strings.Contains(joined, "Button:Save") || !strings.Contains(joined, "TextField:") {
		t.Fatalf("interactive elements missing: %s", joined)
	}
	if strings.Contains(joined, "StaticText") {
		t.Fatalf("non-interactive leaf kept: %s", joined)
	}
	// The wrapper Group is retained as the ancestor of kept nodes.
	if !strings.Contains(joined, "Other:") {
		t.Fatalf("ancestor wrapper dropped: %s", joined)
	}
}

func TestFilterRoles(t *testing.T) {
	out := filterNodes(filterFixture(), treeFilter{roles: map[string]bool{"Button": true}})
	var buttons int
	walkNodes(out, func(n *Node) {
		if n.Role == "Button" {
			buttons++
		}
	})
	if buttons != 1 {
		t.Fatalf("buttons kept = %d, want 1", buttons)
	}
	var fields int
	walkNodes(out, func(n *Node) {
		if n.Role == "TextField" {
			fields++
		}
	})
	if fields != 0 {
		t.Fatalf("TextField should be filtered out")
	}
}

func TestFilterExcludeChrome(t *testing.T) {
	out := filterNodes(filterFixture(), treeFilter{excludeChrome: true})
	walkNodes(out, func(n *Node) {
		if n.Role == "Keyboard" || n.Role == "Key" || isScrollIndicator(n) {
			t.Fatalf("chrome node kept: %s %q", n.Role, n.Label)
		}
	})
	// App content untouched.
	var save bool
	walkNodes(out, func(n *Node) { save = save || n.Label == "Save" })
	if !save {
		t.Fatalf("app content dropped by chrome filter")
	}
}

func TestFilterDoesNotMutateInput(t *testing.T) {
	in := filterFixture()
	before := TreeHash(in)
	filterNodes(in, treeFilter{interactiveOnly: true, excludeChrome: true})
	if TreeHash(in) != before {
		t.Fatalf("filterNodes mutated its input")
	}
}

func TestTreeFilterFromPayloadValidation(t *testing.T) {
	if _, err := treeFilterFromPayload(map[string]any{"roles": []any{}}); err == nil {
		t.Fatalf("empty roles should be rejected")
	}
	if _, err := treeFilterFromPayload(map[string]any{"roles": "Button"}); err == nil {
		t.Fatalf("non-array roles should be rejected")
	}
	if _, err := treeFilterFromPayload(map[string]any{"roles": []any{42}}); err == nil {
		t.Fatalf("non-string role should be rejected")
	}
	if _, err := treeFilterFromPayload(map[string]any{"interactive_only": "yes"}); err == nil {
		t.Fatalf("non-bool interactive_only should be rejected")
	}
	f, err := treeFilterFromPayload(map[string]any{"roles": []any{"Button", "Cell"}, "interactive_only": true})
	if err != nil || !f.roles["Cell"] || !f.interactiveOnly {
		t.Fatalf("valid payload rejected: %+v %v", f, err)
	}
}

func TestScopeSubtree(t *testing.T) {
	nodes := filterFixture()
	out, err := scopeSubtree(map[string]any{"scope": map[string]any{"accessibility_id": "form"}}, nodes, auditViewport)
	if err != nil {
		t.Fatalf("scope failed: %v", err)
	}
	if len(out) != 1 || out[0].Identifier != "form" {
		t.Fatalf("scope returned %+v", out)
	}
	if _, err := scopeSubtree(map[string]any{"scope": map[string]any{"text": "Nope"}}, nodes, auditViewport); err == nil {
		t.Fatalf("no-match scope should error")
	}
	if _, err := scopeSubtree(map[string]any{"scope": "form"}, nodes, auditViewport); err == nil {
		t.Fatalf("non-object scope should be rejected")
	}
	same, err := scopeSubtree(map[string]any{}, nodes, auditViewport)
	if err != nil || len(same) != len(nodes) {
		t.Fatalf("absent scope must return the tree unchanged")
	}
}

func TestCompactTreeText(t *testing.T) {
	text := compactTreeText(filterFixture())
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) != countNodes(filterFixture()) {
		t.Fatalf("lines = %d, want one per node (%d)", len(lines), countNodes(filterFixture()))
	}
	if !strings.HasPrefix(lines[0], "[0] ") {
		t.Fatalf("first line missing [0] index: %q", lines[0])
	}
	if !strings.Contains(text, `[2] Button "Save" (20,100 100x44) interactive`) {
		t.Fatalf("compact line format changed:\n%s", text)
	}
	// Depth is shown by indentation.
	if !strings.HasPrefix(lines[1], "  [1]") {
		t.Fatalf("child not indented: %q", lines[1])
	}
	// Smaller than the nested JSON of the same tree.
	b, _ := json.Marshal(filterFixture())
	if len(text) >= len(b) {
		t.Fatalf("compact (%d bytes) not smaller than JSON (%d bytes)", len(text), len(b))
	}
	// Deterministic across runs.
	for i := 0; i < 3; i++ {
		if compactTreeText(filterFixture()) != text {
			t.Fatalf("compact rendering is not deterministic")
		}
	}
}

func TestObserveFormatValidation(t *testing.T) {
	for payload, want := range map[*map[string]any]string{
		{}:                    "json",
		{"format": "json"}:    "json",
		{"format": "compact"}: "compact",
	} {
		got, err := observeFormat(*payload)
		if err != nil || got != want {
			t.Fatalf("observeFormat(%v) = %q, %v; want %q", *payload, got, err, want)
		}
	}
	if _, err := observeFormat(map[string]any{"format": "yaml"}); err == nil {
		t.Fatalf("unknown format should be rejected")
	}
}
