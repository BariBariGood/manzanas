package actions

import (
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Structured predicate DSL: an alternative to the flat matcher fields,
// carried in the payload's "predicate" object. It matches by exact text,
// substring, or regex, by element type or accessibility id, and by
// position (bounds_hint), spatial relation (near), or structure
// (parent_of). Unlike the flat matcher's best-match ranking, the DSL is
// strict: several matches without an index is an explicit
// ambiguous_match error listing the candidates, never a silent pick.

// dslMaxDepth bounds predicate nesting (near/parent_of recursion).
const dslMaxDepth = 4

// dslCandidateCap bounds how many candidates an ambiguous_match error
// lists before eliding the rest.
const dslCandidateCap = 8

// dslMaxRegexLen bounds text_regex source length so a pathological
// pattern cannot burn CPU/memory compiling and matching on every poll.
const dslMaxRegexLen = 512

// boundsHints are the accepted bounds_hint values. center is the middle
// half of the viewport in both dimensions.
var boundsHints = map[string]bool{
	"top_half": true, "bottom_half": true, "left_half": true,
	"right_half": true, "center": true,
}

// nearDirections normalizes the accepted near.direction values.
var nearDirections = map[string]string{
	"left": "left", "right": "right", "above": "above", "below": "below",
	"up": "above", "down": "below",
}

// dslPredicate is one parsed predicate node.
type dslPredicate struct {
	Text            string
	TextContains    string
	TextRegexSrc    string
	TextRegex       *regexp.Regexp
	Type            string
	AccessibilityID string
	Index           *int
	BoundsHint      string
	Near            *nearSpec
	ParentOf        *dslPredicate
}

// nearSpec is a spatial relation to another uniquely-resolved element:
// the candidate's frame centre must lie in the given direction from the
// anchor's centre, within max_distance points when set.
type nearSpec struct {
	Predicate   *dslPredicate
	Direction   string
	MaxDistance float64 // 0 = unbounded
}

// dslPredicateKeys are the recognized predicate object fields, used to
// reject typos explicitly instead of silently matching everything.
var dslPredicateKeys = map[string]bool{
	"text": true, "text_contains": true, "text_regex": true, "type": true,
	"accessibility_id": true, "index": true, "bounds_hint": true,
	"near": true, "parent_of": true,
}

// dslFromMap parses one predicate object.
func dslFromMap(m map[string]any, depth int) (*dslPredicate, error) {
	if depth >= dslMaxDepth {
		return nil, badRequest("predicate nesting (near/parent_of) is limited to %d levels", dslMaxDepth)
	}
	for k := range m {
		if !dslPredicateKeys[k] {
			return nil, badRequest("unknown predicate field %q (known fields: text, text_contains, text_regex, type, accessibility_id, index, bounds_hint, near, parent_of)", k)
		}
	}
	d := &dslPredicate{
		Text:            strField(m, "text"),
		TextContains:    strField(m, "text_contains"),
		TextRegexSrc:    strField(m, "text_regex"),
		Type:            strField(m, "type"),
		AccessibilityID: strField(m, "accessibility_id"),
	}
	for _, k := range []string{"text", "text_contains", "text_regex", "type", "accessibility_id", "bounds_hint"} {
		if v, ok := m[k]; ok {
			if _, isStr := v.(string); !isStr {
				return nil, badRequest("predicate field %q must be a string", k)
			}
		}
	}
	textForms := 0
	for _, v := range []string{d.Text, d.TextContains, d.TextRegexSrc} {
		if v != "" {
			textForms++
		}
	}
	if textForms > 1 {
		return nil, badRequest("predicate fields text, text_contains, and text_regex are mutually exclusive")
	}
	if d.TextRegexSrc != "" {
		if len(d.TextRegexSrc) > dslMaxRegexLen {
			return nil, badRequest("predicate field %q is limited to %d bytes", "text_regex", dslMaxRegexLen)
		}
		re, err := regexp.Compile(d.TextRegexSrc)
		if err != nil {
			return nil, badRequest("predicate field %q is not a valid regular expression: %v", "text_regex", err)
		}
		d.TextRegex = re
	}
	if v, ok := m["index"]; ok {
		n, err := toNum(v)
		if err != nil || n < 0 || n != math.Trunc(n) {
			return nil, badRequest("predicate field %q must be a non-negative integer", "index")
		}
		i := int(n)
		d.Index = &i
	}
	if hint := strField(m, "bounds_hint"); hint != "" {
		if !boundsHints[hint] {
			return nil, badRequest("predicate field %q must be one of: top_half, bottom_half, left_half, right_half, center", "bounds_hint")
		}
		d.BoundsHint = hint
	}
	if raw, ok := m["near"]; ok {
		nm, ok := raw.(map[string]any)
		if !ok {
			return nil, badRequest("predicate field %q must be an object with predicate, direction, and optional max_distance", "near")
		}
		near, err := nearFromMap(nm, depth+1)
		if err != nil {
			return nil, err
		}
		d.Near = near
	}
	if raw, ok := m["parent_of"]; ok {
		pm, ok := raw.(map[string]any)
		if !ok {
			return nil, badRequest("predicate field %q must be a predicate object", "parent_of")
		}
		child, err := dslFromMap(pm, depth+1)
		if err != nil {
			return nil, err
		}
		d.ParentOf = child
	}
	if !d.hasSelector() {
		return nil, badRequest("predicate requires at least one of: text, text_contains, text_regex, type, accessibility_id, bounds_hint, near, parent_of")
	}
	return d, nil
}

func nearFromMap(m map[string]any, depth int) (*nearSpec, error) {
	for k := range m {
		switch k {
		case "predicate", "direction", "max_distance":
		default:
			return nil, badRequest("unknown near field %q (known fields: predicate, direction, max_distance)", k)
		}
	}
	pm, ok := m["predicate"].(map[string]any)
	if !ok {
		return nil, badRequest("near field %q must be a predicate object", "predicate")
	}
	inner, err := dslFromMap(pm, depth)
	if err != nil {
		return nil, err
	}
	dir, err := enumField(m, "direction")
	if err != nil {
		return nil, badRequest("near field %q must be one of: left, right, above, below", "direction")
	}
	norm, ok := nearDirections[dir]
	if !ok {
		return nil, badRequest("near field %q must be one of: left, right, above, below", "direction")
	}
	n := &nearSpec{Predicate: inner, Direction: norm}
	if v, ok := m["max_distance"]; ok {
		dist, err := toNum(v)
		if err != nil || dist <= 0 {
			return nil, badRequest("near field %q must be a positive number of points", "max_distance")
		}
		n.MaxDistance = dist
	}
	return n, nil
}

// hasSelector reports whether the predicate constrains anything (index
// alone is not a selector).
func (d *dslPredicate) hasSelector() bool {
	return d.Text != "" || d.TextContains != "" || d.TextRegexSrc != "" ||
		d.Type != "" || d.AccessibilityID != "" || d.BoundsHint != "" ||
		d.Near != nil || d.ParentOf != nil
}

// resolve implements elementMatcher: exactly one element or an explicit
// error. Zero matches is errNoMatch (the caller's poll loop keeps
// polling and reports a timeout); several matches without index is an
// ambiguous_match error listing the candidates.
func (d *dslPredicate) resolve(nodes []*Node, viewport *Frame) (*Node, error) {
	cands, err := d.candidates(nodes, viewport)
	if err != nil {
		return nil, err
	}
	if d.Index != nil {
		if *d.Index >= len(cands) {
			return nil, errNoMatch
		}
		return cands[*d.Index], nil
	}
	switch len(cands) {
	case 0:
		return nil, errNoMatch
	case 1:
		return cands[0], nil
	}
	return nil, ambiguousMatch(d, cands, viewport)
}

// candidates returns every node matching the predicate (before index
// selection), in depth-first order.
func (d *dslPredicate) candidates(nodes []*Node, viewport *Frame) ([]*Node, error) {
	region := d.boundsRegion(nodes, viewport)
	var cands []*Node
	if d.ParentOf != nil {
		desc, err := d.ParentOf.candidates(nodes, viewport)
		if err != nil {
			return nil, err
		}
		if d.ParentOf.Index != nil {
			if *d.ParentOf.Index >= len(desc) {
				desc = nil
			} else {
				desc = desc[*d.ParentOf.Index : *d.ParentOf.Index+1]
			}
		}
		cands = d.parentsOf(nodes, desc, region)
	} else {
		walkNodes(nodes, func(n *Node) {
			if d.matchesFields(n, region) {
				cands = append(cands, n)
			}
		})
	}
	if d.Near != nil {
		anchor, err := d.Near.Predicate.resolve(nodes, viewport)
		if err != nil {
			if err == errNoMatch {
				return nil, errNoMatch
			}
			return nil, err
		}
		cands = filterNear(cands, anchor, d.Near)
	}
	return cands, nil
}

// parentsOf returns the candidate ancestors of the matched descendants:
// when the predicate carries its own field constraints, every
// constrained node that contains a matched descendant; otherwise the
// direct parents of the matches.
func (d *dslPredicate) parentsOf(nodes []*Node, desc []*Node, region *Frame) []*Node {
	if len(desc) == 0 {
		return nil
	}
	descSet := make(map[*Node]bool, len(desc))
	for _, n := range desc {
		descSet[n] = true
	}
	constrained := d.Text != "" || d.TextContains != "" || d.TextRegexSrc != "" ||
		d.Type != "" || d.AccessibilityID != "" || d.BoundsHint != ""
	var cands []*Node
	seen := map[*Node]bool{}
	if constrained {
		walkNodes(nodes, func(n *Node) {
			if d.matchesFields(n, region) && n.containsAny(descSet) && !seen[n] {
				seen[n] = true
				cands = append(cands, n)
			}
		})
		return cands
	}
	parents := parentMap(nodes)
	walkNodes(nodes, func(n *Node) {
		if descSet[n] {
			if p := parents[n]; p != nil && !seen[p] {
				seen[p] = true
				cands = append(cands, p)
			}
		}
	})
	return cands
}

// containsAny reports whether any strict descendant of n is in set.
func (n *Node) containsAny(set map[*Node]bool) bool {
	for _, c := range n.Children {
		if set[c] || c.containsAny(set) {
			return true
		}
	}
	return false
}

// parentMap builds child→parent pointers for the whole tree.
func parentMap(nodes []*Node) map[*Node]*Node {
	parents := map[*Node]*Node{}
	var walk func(n *Node)
	walk = func(n *Node) {
		for _, c := range n.Children {
			parents[c] = n
			walk(c)
		}
	}
	for _, n := range nodes {
		walk(n)
	}
	return parents
}

// walkNodes visits every node depth-first.
func walkNodes(nodes []*Node, fn func(*Node)) {
	for _, n := range nodes {
		fn(n)
		walkNodes(n.Children, fn)
	}
}

// matchesFields applies the predicate's own field constraints (not near/
// parent_of) to one node. Text forms match the node's label or value —
// the element's visible text either way UIKit surfaces it.
func (d *dslPredicate) matchesFields(n *Node, region *Frame) bool {
	if d.Type != "" && n.Role != d.Type {
		return false
	}
	if d.AccessibilityID != "" && n.Identifier != d.AccessibilityID {
		return false
	}
	if d.Text != "" && n.Label != d.Text && n.Value != d.Text {
		return false
	}
	if d.TextContains != "" && !strings.Contains(n.Label, d.TextContains) &&
		!strings.Contains(n.Value, d.TextContains) {
		return false
	}
	if d.TextRegex != nil && !d.TextRegex.MatchString(n.Label) &&
		!d.TextRegex.MatchString(n.Value) {
		return false
	}
	if d.BoundsHint != "" {
		if region == nil || n.Frame == nil || n.Frame.W <= 0 || n.Frame.H <= 0 {
			return false
		}
		cx, cy := n.Frame.Center()
		if !inBoundsRegion(cx, cy, *region, d.BoundsHint) {
			return false
		}
	}
	return true
}

// boundsRegion picks the reference rectangle for bounds_hint: the device
// viewport when known, else the union of all node frames.
func (d *dslPredicate) boundsRegion(nodes []*Node, viewport *Frame) *Frame {
	if d.BoundsHint == "" {
		return nil
	}
	if viewport != nil {
		return viewport
	}
	var u *Frame
	walkNodes(nodes, func(n *Node) {
		if n.Frame == nil || n.Frame.W <= 0 || n.Frame.H <= 0 {
			return
		}
		if u == nil {
			f := *n.Frame
			u = &f
			return
		}
		x1 := math.Min(u.X, n.Frame.X)
		y1 := math.Min(u.Y, n.Frame.Y)
		x2 := math.Max(u.X+u.W, n.Frame.X+n.Frame.W)
		y2 := math.Max(u.Y+u.H, n.Frame.Y+n.Frame.H)
		u = &Frame{X: x1, Y: y1, W: x2 - x1, H: y2 - y1}
	})
	return u
}

// inBoundsRegion reports whether a point satisfies a bounds hint within
// the reference rectangle.
func inBoundsRegion(x, y float64, r Frame, hint string) bool {
	switch hint {
	case "top_half":
		return y < r.Y+r.H/2
	case "bottom_half":
		return y >= r.Y+r.H/2
	case "left_half":
		return x < r.X+r.W/2
	case "right_half":
		return x >= r.X+r.W/2
	case "center":
		return x >= r.X+r.W/4 && x < r.X+3*r.W/4 &&
			y >= r.Y+r.H/4 && y < r.Y+3*r.H/4
	}
	return false
}

// filterNear keeps candidates whose frame centre lies in the given
// direction from the anchor's centre AND whose frame overlaps the
// anchor's extent on the perpendicular axis — "right of Alice" means
// Alice's row, not everything anywhere to the right — within
// max_distance points (centre to centre) when set. The anchor itself
// never matches.
func filterNear(cands []*Node, anchor *Node, near *nearSpec) []*Node {
	if anchor.Frame == nil {
		return nil
	}
	a := anchor.Frame
	ax, ay := a.Center()
	var out []*Node
	for _, c := range cands {
		if c == anchor || c.Frame == nil || c.Frame.W <= 0 || c.Frame.H <= 0 {
			continue
		}
		cx, cy := c.Frame.Center()
		dx, dy := cx-ax, cy-ay
		ok := false
		switch near.Direction {
		case "left":
			ok = dx < 0 && spansOverlap(c.Frame.Y, c.Frame.H, a.Y, a.H)
		case "right":
			ok = dx > 0 && spansOverlap(c.Frame.Y, c.Frame.H, a.Y, a.H)
		case "above":
			ok = dy < 0 && spansOverlap(c.Frame.X, c.Frame.W, a.X, a.W)
		case "below":
			ok = dy > 0 && spansOverlap(c.Frame.X, c.Frame.W, a.X, a.W)
		}
		if !ok {
			continue
		}
		if near.MaxDistance > 0 && math.Hypot(dx, dy) > near.MaxDistance {
			continue
		}
		out = append(out, c)
	}
	return out
}

// spansOverlap reports whether two 1-D intervals intersect.
func spansOverlap(p1, l1, p2, l2 float64) bool {
	return p1 < p2+l2 && p2 < p1+l1
}

// ambiguousMatch builds the explicit multi-match error, listing the
// candidates (ids/labels/frames) so the caller can tighten the predicate
// or pick one with index. Off-viewport candidates are marked so the
// caller knows index-picking one of them needs a scroll first.
func ambiguousMatch(d *dslPredicate, cands []*Node, viewport *Frame) *Error {
	listed := cands
	elided := 0
	if len(listed) > dslCandidateCap {
		elided = len(listed) - dslCandidateCap
		listed = listed[:dslCandidateCap]
	}
	offscreen := 0
	var lines []string
	for i, n := range listed {
		line := fmt.Sprintf("[%d] %s", i, describeNode(n))
		if n.Frame != nil && offFrame(n.Frame, viewport) {
			line += " (off-screen)"
			offscreen++
		}
		lines = append(lines, line)
	}
	if elided > 0 {
		lines = append(lines, fmt.Sprintf("... and %d more", elided))
	}
	msg := fmt.Sprintf(
		"predicate (%s) matched %d elements; add index or tighten the predicate (text/type/accessibility_id/bounds_hint/near). Candidates: %s",
		d, len(cands), strings.Join(lines, "; "))
	if offscreen > 0 {
		msg += " — off-screen candidates need scroll_to_element before they can be tapped"
	}
	return &Error{Code: "ambiguous_match", Message: msg}
}

// describeNode renders one candidate for the ambiguity listing.
func describeNode(n *Node) string {
	var parts []string
	if n.Role != "" {
		parts = append(parts, n.Role)
	}
	if n.Label != "" {
		parts = append(parts, fmt.Sprintf("label=%q", n.Label))
	}
	if n.Value != "" {
		parts = append(parts, fmt.Sprintf("value=%q", n.Value))
	}
	if n.Identifier != "" {
		parts = append(parts, fmt.Sprintf("id=%q", n.Identifier))
	}
	if n.Frame != nil {
		parts = append(parts, fmt.Sprintf("frame=(%s,%s %sx%s)",
			fmtNum(n.Frame.X), fmtNum(n.Frame.Y), fmtNum(n.Frame.W), fmtNum(n.Frame.H)))
	}
	if len(parts) == 0 {
		return "(empty node)"
	}
	return strings.Join(parts, " ")
}

// String renders the predicate for error messages.
func (d *dslPredicate) String() string {
	var parts []string
	for _, kv := range []struct{ k, v string }{
		{"text", d.Text}, {"text_contains", d.TextContains},
		{"text_regex", d.TextRegexSrc}, {"type", d.Type},
		{"accessibility_id", d.AccessibilityID}, {"bounds_hint", d.BoundsHint},
	} {
		if kv.v != "" {
			parts = append(parts, fmt.Sprintf("%s=%q", kv.k, kv.v))
		}
	}
	if d.Index != nil {
		parts = append(parts, fmt.Sprintf("index=%d", *d.Index))
	}
	if d.Near != nil {
		s := fmt.Sprintf("near={%s %s", d.Near.Direction, d.Near.Predicate)
		if d.Near.MaxDistance > 0 {
			s += fmt.Sprintf(" max_distance=%s", fmtNum(d.Near.MaxDistance))
		}
		parts = append(parts, s+"}")
	}
	if d.ParentOf != nil {
		parts = append(parts, fmt.Sprintf("parent_of={%s}", d.ParentOf))
	}
	return strings.Join(parts, " ")
}
