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

// elementDriver abstracts the observe/tap/type primitives the composite
// and wait actions are built on, so they run unchanged against the
// simulator backend (AXe/warm helper) and the device backend (WDA).
type elementDriver interface {
	// observeOnce returns one compacted a11y tree poll; transient
	// bridge/agent races come back as errNotYet so wait loops keep
	// polling instead of failing.
	observeOnce(ctx context.Context, udid string) ([]*Node, error)
	// tapXY taps at screen coordinates (points).
	tapXY(ctx context.Context, udid string, x, y float64) error
	// typeInto types text into the focused element, returning the
	// rune count.
	typeInto(ctx context.Context, udid, text string) (int, error)
}

// handleTapElement finds the first element matching the payload predicate
// (label/role/value/id/exact/in_frame) and taps its frame centre.
func handleTapElement(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	return elemTap(ctx, b, udid, p)
}

func elemTap(ctx context.Context, d elementDriver, udid string, p map[string]any) (map[string]any, error) {
	found, res, err := findElement(ctx, d, udid, p)
	if err != nil {
		return nil, err
	}
	x, y, err := tapCenter(ctx, d, udid, found)
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
	found, res, err := findElement(ctx, d, udid, p)
	if err != nil {
		return nil, err
	}
	x, y, err := tapCenter(ctx, d, udid, found)
	if err != nil {
		return nil, err
	}
	typed, err := d.typeInto(ctx, udid, text)
	if err != nil {
		return nil, err
	}
	res["element"] = leafCopy(found)
	res["x"], res["y"] = x, y
	res["typed_runes"] = typed
	return res, nil
}

// findElement polls the a11y tree until an element matching the payload
// predicate appears or the timeout budget runs out, returning the match
// plus the base result fields (elapsed_ms, polls).
func findElement(ctx context.Context, d elementDriver, udid string, p map[string]any) (*Node, map[string]any, error) {
	pr, err := predicateFromPayload(p)
	if err != nil {
		return nil, nil, err
	}
	timeout, interval, err := waitParams(p, defaultWaitTimeout)
	if err != nil {
		return nil, nil, err
	}
	var found *Node
	polls, elapsed, err := pollUntil(ctx, timeout, interval, func(ctx context.Context) error {
		nodes, err := d.observeOnce(ctx, udid)
		if err != nil {
			return err
		}
		hit := pr.find(nodes)
		if hit == nil {
			return errNotYet
		}
		found = hit
		return nil
	})
	if err != nil {
		if errors.Is(err, errNotYet) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, timeoutErr("element (%s) did not appear within the %s budget (%d poll(s), %s elapsed)",
				pr, timeout, polls, elapsed.Round(time.Millisecond))
		}
		return nil, nil, err
	}
	return found, map[string]any{
		"elapsed_ms": elapsed.Milliseconds(),
		"polls":      polls,
	}, nil
}

// tapCenter taps an element's frame centre.
func tapCenter(ctx context.Context, d elementDriver, udid string, n *Node) (float64, float64, error) {
	if n.Frame == nil || n.Frame.W <= 0 || n.Frame.H <= 0 {
		return 0, 0, badRequest("matched element has no tappable frame; refine the predicate")
	}
	x, y := n.Frame.Center()
	if x < 0 || y < 0 {
		return 0, 0, badRequest("matched element is off-screen (centre %s,%s); scroll it into view first", fmtNum(x), fmtNum(y))
	}
	if err := d.tapXY(ctx, udid, x, y); err != nil {
		return 0, 0, err
	}
	return x, y, nil
}
