package actions

import (
	"errors"
	"fmt"
)

// elementMatcher is what the element actions (tap_element,
// type_into_element, wait_for_element, scroll_to_element) resolve a node
// with: either the flat matcher fields (predicate, best-match ranked) or
// the structured predicate DSL (dslPredicate, strict single-match).
type elementMatcher interface {
	fmt.Stringer
	// resolve returns the matched node, errNoMatch when nothing matches
	// (the caller keeps polling), or a hard error (ambiguous_match, bad
	// input) that stops the poll.
	resolve(nodes []*Node, viewport *Frame) (*Node, error)
}

// errNoMatch is returned by resolve when no element matches; poll loops
// treat it like errNotYet and keep polling until their budget runs out.
var errNoMatch = errors.New("no element matches")

// matcherFromPayload picks the matcher form: a "predicate" object selects
// the structured DSL, otherwise the flat matcher fields apply. The two
// forms cannot be combined in one payload.
func matcherFromPayload(p map[string]any) (elementMatcher, error) {
	raw, ok := p["predicate"]
	if !ok || raw == nil {
		pr, err := predicateFromPayload(p)
		if err != nil {
			return nil, err
		}
		return pr, nil
	}
	for _, k := range []string{"label", "role", "value", "id", "placeholder", "exact", "match", "in_frame"} {
		v, dup := p[k]
		if !dup {
			continue
		}
		// Tolerate no-op values (empty string, false, nil): clients often
		// spell out optional fields at their documented defaults.
		switch t := v.(type) {
		case nil:
			continue
		case string:
			if t == "" {
				continue
			}
		case bool:
			if !t {
				continue
			}
		}
		return nil, badRequest("payload field %q cannot be combined with the flat matcher field %q; use one form or the other", "predicate", k)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, badRequest("payload field %q must be a predicate object", "predicate")
	}
	return dslFromMap(m, 0)
}

// resolve implements elementMatcher for the flat matcher: the ranked
// best match, or errNoMatch.
func (pr predicate) resolve(nodes []*Node, viewport *Frame) (*Node, error) {
	if hit := pr.findBest(nodes, viewport); hit != nil {
		return hit, nil
	}
	return nil, errNoMatch
}
