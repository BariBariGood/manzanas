package actions

import (
	"fmt"
	"strings"
)

// Off-screen candidate hints: when a matcher times out finding nothing,
// the elements it WOULD match often exist in the tree but sit outside
// the viewport (scroll views keep off-screen rows resident) or outside
// the matcher's own positional constraint (in_frame / bounds_hint).
// Reporting them tells an agent to scroll instead of guessing that the
// element does not exist. Everything here is deterministic: candidates
// come back in depth-first order and the rendering is stable.

// offscreenHintCap bounds how many off-screen candidates a hint lists.
const offscreenHintCap = 3

// offscreenReporter is implemented by matchers that can list candidates
// which satisfy everything EXCEPT being on screen / inside the
// positional constraint.
type offscreenReporter interface {
	offscreenCandidates(nodes []*Node, viewport *Frame) []*Node
}

// offscreenCandidates for the flat predicate: nodes matching the
// predicate with in_frame relaxed, whose frame centre is outside the
// viewport or outside the requested in_frame rectangle.
func (pr predicate) offscreenCandidates(nodes []*Node, viewport *Frame) []*Node {
	relaxed := pr
	relaxed.InFrame = nil
	var out []*Node
	walkNodes(nodes, func(n *Node) {
		if !relaxed.matches(n) {
			return
		}
		if n.Frame == nil || n.Frame.W <= 0 || n.Frame.H <= 0 {
			return
		}
		if offFrame(n.Frame, viewport) || (pr.InFrame != nil && !pr.matches(n)) {
			out = append(out, n)
		}
	})
	return out
}

// offscreenCandidates for the DSL: candidates with bounds_hint relaxed
// that are off the viewport or excluded by the original bounds_hint.
// near/parent_of stay in force — they are structural, not positional-
// on-screen constraints.
func (d *dslPredicate) offscreenCandidates(nodes []*Node, viewport *Frame) []*Node {
	relaxed := *d
	relaxed.BoundsHint = ""
	cands, err := relaxed.candidates(nodes, viewport)
	if err != nil {
		return nil
	}
	strictSet := map[*Node]bool{}
	if strict, err := d.candidates(nodes, viewport); err == nil {
		for _, n := range strict {
			strictSet[n] = true
		}
	}
	var out []*Node
	for _, n := range cands {
		if n.Frame == nil || n.Frame.W <= 0 || n.Frame.H <= 0 {
			continue
		}
		if offFrame(n.Frame, viewport) || !strictSet[n] {
			out = append(out, n)
		}
	}
	return out
}

// offFrame reports whether a frame's centre lies outside the viewport.
// Unknown viewport means nothing can be called off-screen.
func offFrame(f *Frame, vp *Frame) bool {
	if vp == nil {
		return false
	}
	cx, cy := f.Center()
	return cx < vp.X || cx >= vp.X+vp.W || cy < vp.Y || cy >= vp.Y+vp.H
}

// offDirection says where an off-viewport frame sits relative to the
// viewport, for the scroll hint. Vertical wins ties: vertical scrolling
// is the common case.
func offDirection(f *Frame, vp *Frame) string {
	if vp == nil {
		return ""
	}
	cx, cy := f.Center()
	switch {
	case cy >= vp.Y+vp.H:
		return "below the viewport"
	case cy < vp.Y:
		return "above the viewport"
	case cx >= vp.X+vp.W:
		return "right of the viewport"
	case cx < vp.X:
		return "left of the viewport"
	}
	return ""
}

// offscreenHint renders the near-miss candidate suffix for a no-match
// timeout message, or "" when there is nothing useful to say. The
// suffix starts with "; " so callers can append it directly. Genuinely
// off-viewport candidates get scroll guidance; candidates that are on
// screen but excluded by the matcher's own in_frame/bounds_hint get
// region guidance instead.
func offscreenHint(m elementMatcher, nodes []*Node, viewport *Frame) string {
	r, ok := m.(offscreenReporter)
	if !ok || len(nodes) == 0 {
		return ""
	}
	cands := r.offscreenCandidates(nodes, viewport)
	if len(cands) == 0 {
		return ""
	}
	var off, region []*Node
	for _, n := range cands {
		if offFrame(n.Frame, viewport) {
			off = append(off, n)
		} else {
			region = append(region, n)
		}
	}
	var parts []string
	if len(off) > 0 {
		lines := candidateLines(off, func(n *Node) string { return offDirection(n.Frame, viewport) })
		parts = append(parts, fmt.Sprintf("%d matching element(s) exist off-screen: %s — scroll to bring them into view (scroll_to_element)",
			len(off), strings.Join(lines, "; ")))
	}
	if len(region) > 0 {
		lines := candidateLines(region, func(*Node) string { return "" })
		parts = append(parts, fmt.Sprintf("%d matching element(s) are on screen but outside the requested in_frame/bounds_hint region: %s — relax or adjust the region",
			len(region), strings.Join(lines, "; ")))
	}
	return "; " + strings.Join(parts, "; ")
}

// candidateLines renders up to offscreenHintCap candidate descriptions,
// each optionally annotated by note, with an elision tail.
func candidateLines(cands []*Node, note func(*Node) string) []string {
	listed := cands
	elided := 0
	if len(listed) > offscreenHintCap {
		elided = len(listed) - offscreenHintCap
		listed = listed[:offscreenHintCap]
	}
	var lines []string
	for _, n := range listed {
		desc := describeNode(n)
		if s := note(n); s != "" {
			desc += " (" + s + ")"
		}
		lines = append(lines, desc)
	}
	if elided > 0 {
		lines = append(lines, fmt.Sprintf("... and %d more", elided))
	}
	return lines
}
