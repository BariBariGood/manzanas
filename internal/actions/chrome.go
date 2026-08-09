package actions

import (
	"math"
	"strings"
)

// System-chrome classification: deterministic role/id/frame heuristics
// that recognize elements the OS draws rather than the app under test —
// scroll-indicator pseudo-elements, the on-screen keyboard, and the
// status bar. The audit suppresses them from findings by default
// (include_system_chrome:true restores them) and observe/ui_tree can
// drop them with exclude_system_chrome:true. Heuristics are documented
// here and in docs/mcp.md / proto/PROTOCOL.md; anything not matched
// below is treated as app content.

const (
	// chromeScrollbarMaxThickness is the widest a scroll-indicator
	// overlay gets in points; iOS draws them ~3–8pt thick, real sliders
	// (volume, brightness) are far thicker and labeled.
	chromeScrollbarMaxThickness = 12
	// chromeScrollbarMinElongation is how many times longer than thick a
	// frame must be to read as a scroll-indicator bar rather than a
	// small square control.
	chromeScrollbarMinElongation = 3
)

// isScrollIndicator recognizes UIScrollView's scroll-indicator
// pseudo-elements. The a11y bridge surfaces them two ways: labeled ones
// carry UIKit's own "Vertical scroll bar, N pages" / "Horizontal scroll
// bar" accessibility label (matched by name, any geometry), and
// unlabeled, id-less Slider/ScrollBar leaves are matched by their thin
// elongated frame. They are not tappable controls, so flagging them as
// small touch targets is noise.
func isScrollIndicator(n *Node) bool {
	if n.Role != "Slider" && n.Role != "ScrollBar" {
		return false
	}
	l := strings.ToLower(n.Label)
	if strings.HasPrefix(l, "vertical scroll bar") || strings.HasPrefix(l, "horizontal scroll bar") {
		return true
	}
	if n.Label != "" || n.Identifier != "" || len(n.Children) > 0 {
		return false
	}
	f := n.Frame
	if f == nil || f.W <= 0 || f.H <= 0 {
		return false
	}
	thin := math.Min(f.W, f.H)
	long := math.Max(f.W, f.H)
	return thin <= chromeScrollbarMaxThickness && long >= chromeScrollbarMinElongation*thin
}

// isChromeContainer recognizes subtree roots the OS owns: the on-screen
// keyboard (role Keyboard, plus the input-assistant bar above it) and
// the status bar (role StatusBar, or UIKit's UIStatusBar*/StatusBar
// identifiers). Every descendant of a chrome container is chrome.
func isChromeContainer(n *Node) bool {
	switch n.Role {
	case "Keyboard", "StatusBar":
		return true
	}
	id := n.Identifier
	return id == "SystemInputAssistantView" ||
		strings.HasPrefix(id, "UIStatusBar") || strings.HasPrefix(id, "StatusBar")
}

// isChromeNode reports whether a single node is chrome on its own:
// a chrome container root, a scroll-indicator pseudo-element, or an
// individual keyboard key.
func isChromeNode(n *Node) bool {
	return isChromeContainer(n) || isScrollIndicator(n) || n.Role == "Key"
}

// isSystemChrome reports whether the node is chrome itself or lives
// inside a chrome container, resolving ancestry through parents.
func isSystemChrome(n *Node, parents map[*Node]*Node) bool {
	if isChromeNode(n) {
		return true
	}
	for p := parents[n]; p != nil; p = parents[p] {
		if isChromeContainer(p) {
			return true
		}
	}
	return false
}
