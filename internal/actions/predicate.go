package actions

import (
	"fmt"
	"strings"
)

// predicate selects accessibility nodes by their compacted fields. Role and
// id always match exactly; label and value match by substring unless Exact
// is set. InFrame, when present, additionally requires the node's frame
// centre to lie inside the rectangle.
type predicate struct {
	Label   string
	Role    string
	Value   string
	ID      string
	Exact   bool
	InFrame *Frame
}

// predicateFromPayload reads the matcher fields shared by element-waiting
// actions. At least one of label/role/value/id must be present.
func predicateFromPayload(p map[string]any) (predicate, error) {
	pr := predicate{
		Label: strField(p, "label"),
		Role:  strField(p, "role"),
		Value: strField(p, "value"),
		ID:    strField(p, "id"),
	}
	pr.Exact, _ = p["exact"].(bool)
	if pr.Label == "" && pr.Role == "" && pr.Value == "" && pr.ID == "" {
		return predicate{}, badRequest("element predicate requires at least one of: label, role, value, id")
	}
	if raw, ok := p["in_frame"]; ok {
		m, ok := raw.(map[string]any)
		if !ok {
			return predicate{}, badRequest("payload field %q must be an object with x, y, w, h", "in_frame")
		}
		f := Frame{}
		for _, kv := range []struct {
			key string
			dst *float64
		}{{"x", &f.X}, {"y", &f.Y}, {"w", &f.W}, {"h", &f.H}} {
			v, err := numField(m, kv.key)
			if err != nil {
				return predicate{}, badRequest("in_frame field %q must be a number", kv.key)
			}
			*kv.dst = v
		}
		if f.W <= 0 || f.H <= 0 {
			return predicate{}, badRequest("in_frame requires positive w and h")
		}
		pr.InFrame = &f
	}
	return pr, nil
}

// matches reports whether one node satisfies the predicate.
func (pr predicate) matches(n *Node) bool {
	if pr.Role != "" && n.Role != pr.Role {
		return false
	}
	if pr.ID != "" && n.Identifier != pr.ID {
		return false
	}
	if !matchText(n.Label, pr.Label, pr.Exact) || !matchText(n.Value, pr.Value, pr.Exact) {
		return false
	}
	if pr.InFrame != nil {
		if n.Frame == nil {
			return false
		}
		cx, cy := n.Frame.Center()
		if cx < pr.InFrame.X || cx > pr.InFrame.X+pr.InFrame.W ||
			cy < pr.InFrame.Y || cy > pr.InFrame.Y+pr.InFrame.H {
			return false
		}
	}
	return true
}

// find returns the first depth-first node matching the predicate, or nil.
func (pr predicate) find(nodes []*Node) *Node {
	for _, n := range nodes {
		if pr.matches(n) {
			return n
		}
		if hit := pr.find(n.Children); hit != nil {
			return hit
		}
	}
	return nil
}

// String renders the predicate for error messages.
func (pr predicate) String() string {
	var parts []string
	for _, kv := range []struct{ k, v string }{
		{"label", pr.Label}, {"role", pr.Role}, {"value", pr.Value}, {"id", pr.ID},
	} {
		if kv.v != "" {
			parts = append(parts, fmt.Sprintf("%s=%q", kv.k, kv.v))
		}
	}
	if pr.Exact {
		parts = append(parts, "exact")
	}
	if pr.InFrame != nil {
		parts = append(parts, fmt.Sprintf("in_frame=(%g,%g %gx%g)",
			pr.InFrame.X, pr.InFrame.Y, pr.InFrame.W, pr.InFrame.H))
	}
	return strings.Join(parts, " ")
}

// matchText applies the exact/substring text rule; an empty want always
// matches.
func matchText(have, want string, exact bool) bool {
	if want == "" {
		return true
	}
	if exact {
		return have == want
	}
	return strings.Contains(have, want)
}

func strField(p map[string]any, key string) string {
	s, _ := p[key].(string)
	return s
}
