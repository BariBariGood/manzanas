package actions

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// predicate selects accessibility nodes by their compacted fields. Role and
// id always match exactly; label, value, and placeholder match by substring
// unless Exact is set. Placeholder also matches against the node's value,
// because UIKit surfaces an empty text field's placeholder as its AX value.
// InFrame, when present, additionally requires the node's frame centre to
// lie inside the rectangle.
type predicate struct {
	Label       string
	Role        string
	Value       string
	ID          string
	Placeholder string
	Exact       bool
	InFrame     *Frame
}

// predicateFromPayload reads the matcher fields shared by element-waiting
// actions. At least one of label/role/value/id/placeholder must be present.
func predicateFromPayload(p map[string]any) (predicate, error) {
	pr := predicate{
		Label:       strField(p, "label"),
		Role:        strField(p, "role"),
		Value:       strField(p, "value"),
		ID:          strField(p, "id"),
		Placeholder: strField(p, "placeholder"),
	}
	pr.Exact, _ = p["exact"].(bool)
	match, err := enumField(p, "match")
	if err != nil {
		return predicate{}, badRequest("payload field %q must be %q or %q", "match", "exact", "substring")
	}
	switch match {
	case "":
	case "exact":
		pr.Exact = true
	case "substring":
	default:
		return predicate{}, badRequest("payload field %q must be %q or %q", "match", "exact", "substring")
	}
	if pr.Label == "" && pr.Role == "" && pr.Value == "" && pr.ID == "" && pr.Placeholder == "" {
		return predicate{}, badRequest("element predicate requires at least one of: label, role, value, id, placeholder")
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
	if pr.Placeholder != "" &&
		!matchText(n.Placeholder, pr.Placeholder, pr.Exact) &&
		!matchText(n.Value, pr.Placeholder, pr.Exact) {
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

// matchRank orders candidate matches; lower fields win, compared in order.
type matchRank struct {
	untappable int     // 0 when the node has a positive-size frame
	offscreen  int     // 0 when a valid frame's centre lies inside the viewport
	exact      int     // 0 when every requested text field matches exactly
	inter      int     // 0 for interactable elements (button/link/textfield/...)
	area       float64 // frame area; a missing frame ranks last (untappable)
	order      int     // depth-first position, the final tie-break
}

func (r matchRank) less(o matchRank) bool {
	if r.untappable != o.untappable {
		return r.untappable < o.untappable
	}
	if r.offscreen != o.offscreen {
		return r.offscreen < o.offscreen
	}
	if r.exact != o.exact {
		return r.exact < o.exact
	}
	if r.inter != o.inter {
		return r.inter < o.inter
	}
	if r.area != o.area {
		return r.area < o.area
	}
	return r.order < o.order
}

// findBest returns the best-ranked node matching the predicate, or nil.
// Substring predicates can hit several elements at once (description text
// containing the word, a Group whose label concatenates its children, and
// the button itself); ranking prefers on-viewport elements (scroll views
// keep off-screen duplicates in the tree — a visible match must beat
// them), then exact text matches, then interactable elements, then the
// smallest frame, so the intended control wins over its containers and
// surrounding copy. viewport may be nil (no containment term).
func (pr predicate) findBest(nodes []*Node, viewport *Frame) *Node {
	var best *Node
	var bestRank matchRank
	order := 0
	var walk func([]*Node)
	walk = func(ns []*Node) {
		for _, n := range ns {
			if pr.matches(n) {
				r := pr.rank(n, viewport, order)
				if best == nil || r.less(bestRank) {
					best, bestRank = n, r
				}
				order++
			}
			walk(n.Children)
		}
	}
	walk(nodes)
	return best
}

func (pr predicate) rank(n *Node, viewport *Frame, order int) matchRank {
	r := matchRank{area: math.MaxFloat64, order: order}
	if n.Frame == nil || n.Frame.W <= 0 || n.Frame.H <= 0 {
		// Untappable: never let it outrank a framed candidate.
		r.untappable = 1
	} else if viewport != nil {
		if cx, cy := n.Frame.Center(); cx < viewport.X || cx >= viewport.X+viewport.W ||
			cy < viewport.Y || cy >= viewport.Y+viewport.H {
			r.offscreen = 1
		}
	}
	if !pr.exactHit(n) {
		r.exact = 1
	}
	if !n.Interactable {
		r.inter = 1
	}
	if n.Frame != nil && n.Frame.W > 0 && n.Frame.H > 0 {
		r.area = n.Frame.W * n.Frame.H
	}
	return r
}

// exactHit reports whether every requested text field matches the node
// exactly (not merely by substring).
func (pr predicate) exactHit(n *Node) bool {
	if pr.Label != "" && n.Label != pr.Label {
		return false
	}
	if pr.Value != "" && n.Value != pr.Value {
		return false
	}
	if pr.Placeholder != "" && n.Placeholder != pr.Placeholder && n.Value != pr.Placeholder {
		return false
	}
	return true
}

// String renders the predicate for error messages.
func (pr predicate) String() string {
	var parts []string
	for _, kv := range []struct{ k, v string }{
		{"label", pr.Label}, {"role", pr.Role}, {"value", pr.Value}, {"id", pr.ID},
		{"placeholder", pr.Placeholder},
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

// enumField reads an optional string-enum payload field; a present
// non-string value is an error rather than being silently coerced to
// the default (the caller wraps it in the field-specific bad_request).
func enumField(p map[string]any, key string) (string, error) {
	raw, ok := p[key]
	if !ok {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", errNonString
	}
	return s, nil
}

var errNonString = errors.New("not a string")
