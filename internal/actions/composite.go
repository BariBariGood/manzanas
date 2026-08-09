package actions

import (
	"context"
	"errors"
	"time"
)

// Composite element actions: the server resolves an element predicate and
// taps (and optionally types) in one request, saving the caller a full
// observe round-trip per interaction. The element is polled for like
// wait_for_element (same predicate + timeout_ms/interval_ms fields), so a
// screen still settling doesn't fail the action.

// observation is one a11y tree poll: the compacted nodes plus the device
// viewport (the root a11y element's screen bounds) when the backend
// exposes it, so element taps can be validated against the screen.
type observation struct {
	nodes    []*Node
	viewport *Frame
}

// elementDriver abstracts the observe/tap/type primitives the composite
// and wait actions are built on, so they run unchanged against the
// simulator backend (AXe/warm helper) and the device backend (WDA).
type elementDriver interface {
	// observeOnce returns one compacted a11y tree poll; transient
	// bridge/agent races come back as errNotYet so wait loops keep
	// polling instead of failing. refresh forces the poll to bypass any
	// resident warm helper so the snapshot cannot be served from a stale
	// connection.
	observeOnce(ctx context.Context, udid string, refresh bool) (observation, error)
	// tapXY taps at screen coordinates (points).
	tapXY(ctx context.Context, udid string, x, y float64) error
	// typeInto types text into the focused element, returning the
	// rune count. opts selects the typing strategy; a backend that
	// cannot service the requested strategy rejects it.
	typeInto(ctx context.Context, udid, text string, opts typeOpts) (int, error)
}

// typeOptsValidator is optionally implemented by an elementDriver whose
// typeInto cannot service every typing option (e.g. the device backend
// rejects the simulator-only paste strategy), letting composite actions
// reject a doomed request before any UI mutation.
type typeOptsValidator interface {
	validateTypeOpts(opts typeOpts) error
}

// handleTapElement finds the best element matching the payload predicate
// (label/role/value/id/placeholder/exact/match/in_frame) and taps it at
// the requested anchor (frame centre by default).
func handleTapElement(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	return elemTap(ctx, b, udid, p)
}

func elemTap(ctx context.Context, d elementDriver, udid string, p map[string]any) (map[string]any, error) {
	anchor, err := anchorFromPayload(p)
	if err != nil {
		return nil, err
	}
	found, viewport, res, err := findElement(ctx, d, udid, p, anchor)
	if err != nil {
		return nil, err
	}
	x, y, err := tapAnchor(ctx, d, udid, found, anchor, viewport)
	if err != nil {
		return nil, err
	}
	res["element"] = leafCopy(found)
	res["x"], res["y"] = x, y
	return res, nil
}

// handleTypeIntoElement finds the element, taps its centre to focus it,
// then types the payload text.
func handleTypeIntoElement(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	return elemTypeInto(ctx, b, udid, p)
}

func elemTypeInto(ctx context.Context, d elementDriver, udid string, p map[string]any) (map[string]any, error) {
	text, ok := p["text"].(string)
	if !ok || text == "" {
		return nil, badRequest("type_into_element requires a non-empty string payload field %q", "text")
	}
	anchor, err := anchorFromPayload(p)
	if err != nil {
		return nil, err
	}
	opts, err := typeOptsFromPayload(p)
	if err != nil {
		return nil, err
	}
	// Reject a request the backend can't service before the focusing tap
	// runs, so a doomed request never mutates the UI.
	if v, ok := d.(typeOptsValidator); ok {
		if err := v.validateTypeOpts(opts); err != nil {
			return nil, err
		}
	}
	found, viewport, res, err := findElement(ctx, d, udid, p, anchor)
	if err != nil {
		return nil, err
	}
	x, y, err := tapAnchor(ctx, d, udid, found, anchor, viewport)
	if err != nil {
		return nil, err
	}
	if opts.requireFocus {
		refresh, err := boolFlag(p, "refresh", false)
		if err != nil {
			return nil, err
		}
		// The tap above is what focuses the field; the guard verifies the
		// keyboard actually came up before any keystroke is sent.
		if err := requireFocusedField(ctx, d, udid, refresh); err != nil {
			return nil, err
		}
	}
	typed, err := d.typeInto(ctx, udid, text, opts)
	if err != nil {
		return nil, err
	}
	res["element"] = leafCopy(found)
	res["x"], res["y"] = x, y
	res["typed_runes"] = typed
	if opts.strategy == typeStrategyPaste {
		res["strategy"] = typeStrategyPaste
	}
	return res, nil
}

// findElement polls the a11y tree until an element matching the payload
// predicate appears with its anchor point inside the viewport (or the
// timeout budget runs out), returning the match, the viewport of the poll
// that matched (nil when unknown), and the base result fields
// (elapsed_ms, polls). A match that is only transiently off screen (a
// sheet or toast still animating in) keeps the wait polling; off_viewport
// surfaces only when the budget expires with nothing tappable on screen.
func findElement(ctx context.Context, d elementDriver, udid string, p map[string]any, anchor string) (*Node, *Frame, map[string]any, error) {
	pr, err := matcherFromPayload(p)
	if err != nil {
		return nil, nil, nil, err
	}
	refresh, err := boolFlag(p, "refresh", false)
	if err != nil {
		return nil, nil, nil, err
	}
	timeout, interval, err := waitParams(p, defaultWaitTimeout)
	if err != nil {
		return nil, nil, nil, err
	}
	var found *Node
	var viewport *Frame
	var lastObs observation
	offScreen := false
	polls, elapsed, err := pollUntil(ctx, timeout, interval, func(ctx context.Context) error {
		obs, err := d.observeOnce(ctx, udid, refresh)
		if err != nil {
			offScreen = false
			return err
		}
		lastObs = obs
		hit, rerr := pr.resolve(obs.nodes, obs.viewport)
		if rerr != nil {
			if errors.Is(rerr, errNoMatch) {
				offScreen = false
				return errNotYet
			}
			// A hard resolution error (e.g. ambiguous_match) stops the
			// poll: more waiting cannot disambiguate it.
			return rerr
		}
		if obs.viewport != nil && hit.Frame != nil && hit.Frame.W > 0 && hit.Frame.H > 0 {
			if x, y := anchorPoint(hit.Frame, anchor); !pointInViewport(x, y, obs.viewport) {
				offScreen = true
				return errNotYet
			}
		}
		offScreen = false
		found, viewport = hit, obs.viewport
		return nil
	})
	if err != nil {
		if errors.Is(err, errNotYet) || errors.Is(err, context.DeadlineExceeded) {
			if offScreen {
				return nil, nil, nil, offViewport("element (%s) matched but stayed outside the viewport for the whole %s budget (%d poll(s), %s elapsed); scroll it into view first",
					pr, timeout, polls, elapsed.Round(time.Millisecond))
			}
			return nil, nil, nil, timeoutErr("element (%s) did not appear within the %s budget (%d poll(s), %s elapsed)%s",
				pr, timeout, polls, elapsed.Round(time.Millisecond),
				offscreenHint(pr, lastObs.nodes, lastObs.viewport))
		}
		return nil, nil, nil, err
	}
	return found, viewport, map[string]any{
		"elapsed_ms": elapsed.Milliseconds(),
		"polls":      polls,
	}, nil
}

// anchorFromPayload reads the optional tap-anchor payload field.
func anchorFromPayload(p map[string]any) (string, error) {
	anchor, err := enumField(p, "anchor")
	if err == nil {
		switch anchor {
		case "", "center", "start", "end":
			return anchor, nil
		}
	}
	return "", badRequest("payload field %q must be one of: start, center, end", "anchor")
}

// anchorPoint returns the tap coordinates for a frame: the centre by
// default, or a point inset one half line-height from the leading/trailing
// edge for anchor start/end — for a text element whose label is longer
// than the interesting part (an inline "Sign in" link at the end of a
// sentence), the edge anchors land on the matched text instead of the
// middle of the sentence.
func anchorPoint(f *Frame, anchor string) (float64, float64) {
	x, y := f.Center()
	inset := f.H / 2
	if inset > f.W/2 {
		inset = f.W / 2
	}
	switch anchor {
	case "start":
		x = f.X + inset
	case "end":
		x = f.X + f.W - inset
	}
	return x, y
}

// tapAnchor taps an element at the requested anchor, rejecting taps whose
// coordinates fall outside the device viewport (when known) so an
// off-screen element surfaces an explicit error instead of a silently
// ineffective tap.
func tapAnchor(ctx context.Context, d elementDriver, udid string, n *Node, anchor string, viewport *Frame) (float64, float64, error) {
	if n.Frame == nil || n.Frame.W <= 0 || n.Frame.H <= 0 {
		return 0, 0, badRequest("matched element has no tappable frame; refine the predicate")
	}
	x, y := anchorPoint(n.Frame, anchor)
	if x < 0 || y < 0 {
		return 0, 0, badRequest("matched element is off-screen (tap point %s,%s); scroll it into view first", fmtNum(x), fmtNum(y))
	}
	if viewport != nil && !pointInViewport(x, y, viewport) {
		return 0, 0, offViewport("matched element is outside the %sx%s viewport (tap point %s,%s); scroll it into view first",
			fmtNum(viewport.W), fmtNum(viewport.H), fmtNum(x), fmtNum(y))
	}
	if err := d.tapXY(ctx, udid, x, y); err != nil {
		return 0, 0, err
	}
	return x, y, nil
}

func pointInViewport(x, y float64, vp *Frame) bool {
	return x >= vp.X && x < vp.X+vp.W && y >= vp.Y && y < vp.Y+vp.H
}
