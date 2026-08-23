package actions

import (
	"strings"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
)

// Text-set utilities for movement detection in scrolling composites
// (scroll_until / scroll_collect). A screen's visible text is reduced to a
// normalized set of tokens; comparing before/after sets with Jaccard overlap
// distinguishes real scroll progress (low overlap) from a bounce-back
// overscroll (high overlap). Volatile chrome (clock/status strip at the top,
// home-indicator strip at the bottom) is masked out first so a ticking clock
// does not depress the overlap.

// Defaults for the volatile bands, expressed as fractions of the viewport
// height (absolute pixels would break across window sizes and mirror scales).
const (
	defaultTopBandFrac    = 0.05
	defaultBottomBandFrac = 0.03
)

// textSetOpts tunes normalization and masking.
type textSetOpts struct {
	// minRunLen drops normalized runs shorter than this many runes
	// (OCR noise like "l", "|.", stray punctuation). Default 2.
	minRunLen int
	// minConfidence drops OCR boxes below this confidence. Only applied
	// by ocrTextSet, where per-box confidence is available. Default 0.5.
	minConfidence float64
	// topBandFrac / bottomBandFrac size the volatile bands as fractions
	// of the viewport height. Negative disables the band; zero means the
	// default.
	topBandFrac    float64
	bottomBandFrac float64
}

func (o textSetOpts) withDefaults() textSetOpts {
	if o.minRunLen == 0 {
		o.minRunLen = 2
	}
	if o.minConfidence == 0 {
		o.minConfidence = 0.5
	}
	if o.topBandFrac == 0 {
		o.topBandFrac = defaultTopBandFrac
	}
	if o.bottomBandFrac == 0 {
		o.bottomBandFrac = defaultBottomBandFrac
	}
	return o
}

// normalizeRun lowercases and trims a text run, returning "" when the run is
// too short to be a useful token.
func normalizeRun(s string, minLen int) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len([]rune(s)) < minLen {
		return ""
	}
	return s
}

// inVolatileBand reports whether a box's vertical center falls inside the
// masked top or bottom band of the viewport. A nil viewport (or one with no
// height) disables masking.
func inVolatileBand(box, viewport *Frame, topFrac, bottomFrac float64) bool {
	if box == nil || viewport == nil || viewport.H <= 0 {
		return false
	}
	cy := box.Y + box.H/2
	if topFrac > 0 && cy < viewport.Y+viewport.H*topFrac {
		return true
	}
	if bottomFrac > 0 && cy > viewport.Y+viewport.H*(1-bottomFrac) {
		return true
	}
	return false
}

// textSet flattens a node tree into a normalized set of visible text runs.
// mask is the viewport frame used to strip volatile bands; nil skips masking.
// Nodes carry no OCR confidence (ocrNodes drops it), so confidence filtering
// only applies in ocrTextSet.
func textSet(nodes []*Node, mask *Frame) map[string]struct{} {
	return textSetWith(nodes, mask, textSetOpts{})
}

func textSetWith(nodes []*Node, mask *Frame, opts textSetOpts) map[string]struct{} {
	opts = opts.withDefaults()
	out := make(map[string]struct{})
	var walk func(ns []*Node)
	walk = func(ns []*Node) {
		for _, n := range ns {
			if n == nil {
				continue
			}
			if !inVolatileBand(n.Frame, mask, opts.topBandFrac, opts.bottomBandFrac) {
				for _, s := range []string{n.Label, n.Value, n.Placeholder} {
					if t := normalizeRun(s, opts.minRunLen); t != "" {
						out[t] = struct{}{}
					}
				}
			}
			walk(n.Children)
		}
	}
	walk(nodes)
	return out
}

// ocrTextSet builds a text set straight from OCR boxes, where per-box
// confidence is still available for filtering.
func ocrTextSet(boxes []mirror.OCRBox, mask *Frame, opts textSetOpts) map[string]struct{} {
	opts = opts.withDefaults()
	out := make(map[string]struct{})
	for _, b := range boxes {
		if b.Confidence < opts.minConfidence {
			continue
		}
		f := &Frame{X: b.X, Y: b.Y, W: b.W, H: b.H}
		if inVolatileBand(f, mask, opts.topBandFrac, opts.bottomBandFrac) {
			continue
		}
		if t := normalizeRun(b.Text, opts.minRunLen); t != "" {
			out[t] = struct{}{}
		}
	}
	return out
}

// jaccard returns |a∩b| / |a∪b|. Two empty sets are defined as identical
// (1.0): an empty screen that stays empty has not moved.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	return float64(inter) / float64(union)
}
