package actions

import (
	"fmt"
	"strings"
)

// observe/ui_tree server-side filters and the compact indexed format:
// opt-in payload knobs that cut response size before the tree crosses
// the wire. All filtering is pure and deterministic; the "hash" result
// always digests the FULL tree so change detection stays comparable
// whatever filters a caller uses.

// treeFilter is the parsed filter set for one observe.
type treeFilter struct {
	interactiveOnly bool
	roles           map[string]bool // nil = any role
	excludeChrome   bool
}

// active reports whether any filter would change the tree.
func (f treeFilter) active() bool {
	return f.interactiveOnly || len(f.roles) > 0 || f.excludeChrome
}

// keep reports whether a node itself satisfies the filters (an ancestor
// of a kept node is retained regardless, to preserve tree structure).
func (f treeFilter) keep(n *Node) bool {
	if f.interactiveOnly && !n.Interactable {
		return false
	}
	if len(f.roles) > 0 && !f.roles[n.Role] {
		return false
	}
	return true
}

// filterNodes returns a pruned copy of the tree: nodes that satisfy the
// filters, plus the ancestors needed to keep them attached. With
// excludeChrome, chrome nodes (and whole chrome-container subtrees; see
// chrome.go) are dropped first. The input tree is never mutated.
func filterNodes(nodes []*Node, f treeFilter) []*Node {
	var out []*Node
	for _, n := range nodes {
		if f.excludeChrome && isChromeNode(n) {
			continue
		}
		kids := filterNodes(n.Children, f)
		if !f.keep(n) && len(kids) == 0 {
			continue
		}
		c := *n
		c.Children = kids
		out = append(out, &c)
	}
	return out
}

// treeFilterFromPayload parses the observe filter fields.
func treeFilterFromPayload(p map[string]any) (treeFilter, error) {
	f := treeFilter{}
	var err error
	if f.interactiveOnly, err = boolFlag(p, "interactive_only", false); err != nil {
		return f, err
	}
	if f.excludeChrome, err = boolFlag(p, "exclude_system_chrome", false); err != nil {
		return f, err
	}
	if raw, ok := p["roles"]; ok {
		list, ok := raw.([]any)
		if !ok || len(list) == 0 {
			return f, badRequest("payload field %q must be a non-empty array of role names (e.g. [\"Button\",\"Cell\"])", "roles")
		}
		f.roles = make(map[string]bool, len(list))
		for _, v := range list {
			role, ok := v.(string)
			if !ok || role == "" {
				return f, badRequest("payload field %q must contain non-empty strings", "roles")
			}
			f.roles[role] = true
		}
	}
	return f, nil
}

// scopeSubtree resolves the optional "scope" predicate (the structured
// predicate DSL, resolved strictly like the element actions) and narrows
// the tree to the matched element's subtree.
func scopeSubtree(p map[string]any, nodes []*Node, viewport *Frame) ([]*Node, error) {
	raw, ok := p["scope"]
	if !ok || raw == nil {
		return nodes, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, badRequest("payload field %q must be a predicate object (e.g. {\"type\":\"Table\"})", "scope")
	}
	d, err := dslFromMap(m, 0)
	if err != nil {
		return nil, err
	}
	hit, rerr := d.resolve(nodes, viewport)
	if rerr != nil {
		if rerr == errNoMatch {
			return nil, timeoutErr("observe scope (%s) matched no element%s; drop the scope or adjust the predicate", d,
				offscreenHint(d, nodes, viewport))
		}
		return nil, rerr
	}
	return []*Node{hit}, nil
}

// countNodes counts every node in the tree.
func countNodes(nodes []*Node) int {
	n := 0
	walkNodes(nodes, func(*Node) { n++ })
	return n
}

// compactTreeText renders the tree one line per element, indentation
// showing depth, each prefixed with a stable [i] index assigned in
// depth-first order over the returned (post-filter) tree — the same
// order predicate index picks candidates in. The rendering is
// deterministic: an unchanged screen yields byte-identical output.
func compactTreeText(nodes []*Node) string {
	var b strings.Builder
	i := 0
	var walk func(ns []*Node, depth int)
	walk = func(ns []*Node, depth int) {
		for _, n := range ns {
			b.WriteString(strings.Repeat("  ", depth))
			fmt.Fprintf(&b, "[%d] %s", i, compactLine(n))
			b.WriteByte('\n')
			i++
			walk(n.Children, depth+1)
		}
	}
	walk(nodes, 0)
	return b.String()
}

// compactLine renders one node: role, texts, id, frame, flags.
func compactLine(n *Node) string {
	parts := []string{orRole(n.Role)}
	if n.Label != "" {
		parts = append(parts, fmt.Sprintf("%q", n.Label))
	}
	if n.Value != "" {
		parts = append(parts, fmt.Sprintf("value=%q", n.Value))
	}
	if n.Placeholder != "" {
		parts = append(parts, fmt.Sprintf("placeholder=%q", n.Placeholder))
	}
	if n.Identifier != "" {
		parts = append(parts, fmt.Sprintf("id=%q", n.Identifier))
	}
	if f := n.Frame; f != nil {
		parts = append(parts, fmt.Sprintf("(%s,%s %sx%s)",
			fmtNum(f.X), fmtNum(f.Y), fmtNum(f.W), fmtNum(f.H)))
	}
	if n.Interactable {
		parts = append(parts, "interactive")
	}
	if n.Disabled {
		parts = append(parts, "disabled")
	}
	return strings.Join(parts, " ")
}
