package actions

import (
	"context"
	"errors"
	"time"
)

// scroll_to_element: scroll a container until an element matching the
// payload predicate is inside the viewport and hittable, with bounded
// swipe attempts. The two failure modes are deliberately distinct:
// "never appeared in the tree" (timeout) vs "matched but could not be
// brought into view" (off_viewport), so callers know whether to look on
// another screen or give up on scrolling.

// Scroll defaults and bounds.
const (
	defaultScrollTimeout = 30 * time.Second
	defaultMaxScrolls    = 8
	maxMaxScrolls        = 30
	// scrollSwipeFraction is how far across the scroll region each swipe
	// travels (fraction of the region's height/width). Modest so a single
	// swipe cannot fling past the target.
	scrollSwipeFraction = 0.4
)

// scrollDriver extends elementDriver with the swipe primitive the scroll
// loop needs; both the simulator and device backends implement it.
type scrollDriver interface {
	elementDriver
	// swipeXY swipes between two screen points (points, not pixels).
	swipeXY(ctx context.Context, udid string, startX, startY, endX, endY float64) error
}

func handleScrollToElement(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	return elemScrollTo(ctx, b, udid, p)
}

func elemScrollTo(ctx context.Context, d scrollDriver, udid string, p map[string]any) (map[string]any, error) {
	pr, err := predicateFromPayload(p)
	if err != nil {
		return nil, err
	}
	anchor, err := anchorFromPayload(p)
	if err != nil {
		return nil, err
	}
	refresh, err := boolFlag(p, "refresh", false)
	if err != nil {
		return nil, err
	}
	direction, err := scrollDirection(p)
	if err != nil {
		return nil, err
	}
	maxScrolls := defaultMaxScrolls
	if v, ok := p["max_scrolls"]; ok {
		n, err := toNum(v)
		if err != nil || n < 1 || n > maxMaxScrolls {
			return nil, badRequest("payload field %q must be a number between 1 and %d", "max_scrolls", maxMaxScrolls)
		}
		maxScrolls = int(n)
	}
	timeout, interval, err := waitParams(p, defaultScrollTimeout)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	deadline := start.Add(timeout)
	seen := false
	scrolls, polls := 0, 0
	for {
		if time.Now().After(deadline) {
			return nil, scrollFailure(pr, seen, scrolls, time.Since(start))
		}
		// pollUntil bounds each poll by the remaining budget itself; a
		// deadline-wrapped ctx would defeat its "budget expired counts as
		// a timeout" classification.
		obs, err := observeReadable(ctx, d, udid, refresh, deadline, interval)
		polls++
		if err != nil {
			if errors.Is(err, errNotYet) || errors.Is(err, context.DeadlineExceeded) {
				return nil, scrollFailure(pr, seen, scrolls, time.Since(start))
			}
			return nil, err
		}
		hit := pr.findBest(obs.nodes, obs.viewport)
		region := scrollRegion(obs)
		dir := direction
		if hit != nil {
			seen = true
			if hit.Frame == nil || hit.Frame.W <= 0 || hit.Frame.H <= 0 {
				return nil, badRequest("matched element (%s) has no usable frame, so it cannot be scrolled into view; refine the predicate", pr)
			}
			x, y := anchorPoint(hit.Frame, anchor)
			if obs.viewport == nil || pointInViewport(x, y, obs.viewport) {
				return map[string]any{
					"element":    leafCopy(hit),
					"scrolls":    scrolls,
					"polls":      polls,
					"elapsed_ms": time.Since(start).Milliseconds(),
					"hash":       TreeHash(obs.nodes),
				}, nil
			}
			if toward := directionToward(x, y, obs.viewport); toward != "" {
				dir = toward
			}
		}
		if scrolls >= maxScrolls || time.Now().After(deadline) {
			return nil, scrollFailure(pr, seen, scrolls, time.Since(start))
		}
		x1, y1, x2, y2 := swipeForDirection(region, dir)
		if err := d.swipeXY(ctx, udid, x1, y1, x2, y2); err != nil {
			return nil, err
		}
		scrolls++
		// Let the scroll settle before the next observation.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// observeReadable polls until one readable a11y snapshot is produced or
// the deadline passes, absorbing transient bridge races.
func observeReadable(ctx context.Context, d elementDriver, udid string, refresh bool, deadline time.Time, interval time.Duration) (observation, error) {
	var obs observation
	_, _, err := pollUntil(ctx, time.Until(deadline), interval, func(ctx context.Context) error {
		o, err := d.observeOnce(ctx, udid, refresh)
		if err != nil {
			return err
		}
		obs = o
		return nil
	})
	return obs, err
}

// scrollFailure builds the terminal error: off_viewport when the element
// was seen in the tree but never entered the viewport, timeout when it
// never appeared at all.
func scrollFailure(pr predicate, seen bool, scrolls int, elapsed time.Duration) error {
	if seen {
		return offViewport("element (%s) matched in the tree but could not be brought into the viewport after %d scroll(s) (%s elapsed); it may be pinned off-screen or inside a non-scrollable container — observe the tree to check the layout",
			pr, scrolls, elapsed.Round(time.Millisecond))
	}
	return timeoutErr("element (%s) not found in the accessibility tree after %d scroll(s) (%s elapsed); it may be on a different screen — observe the tree to see what is on screen now",
		pr, scrolls, elapsed.Round(time.Millisecond))
}

// scrollDirection reads the optional "direction" payload field: the edge
// of the content the scroll should reveal (down = content below the fold).
func scrollDirection(p map[string]any) (string, error) {
	dir, err := enumField(p, "direction")
	if err == nil {
		switch dir {
		case "":
			return "down", nil
		case "down", "up", "left", "right":
			return dir, nil
		}
	}
	return "", badRequest("payload field %q must be one of: down, up, left, right", "direction")
}

// directionToward picks the scroll direction that moves the viewport
// toward an off-viewport target point, or "" when it cannot tell.
func directionToward(x, y float64, vp *Frame) string {
	if vp == nil {
		return ""
	}
	switch {
	case y >= vp.Y+vp.H:
		return "down"
	case y < vp.Y:
		return "up"
	case x >= vp.X+vp.W:
		return "right"
	case x < vp.X:
		return "left"
	}
	return ""
}

// scrollRegion picks the rectangle swipes happen in: the viewport when
// known, else the largest root frame, else a conservative phone-sized
// fallback.
func scrollRegion(obs observation) Frame {
	if obs.viewport != nil {
		return *obs.viewport
	}
	var best *Frame
	for _, n := range obs.nodes {
		if n.Frame != nil && n.Frame.W > 0 && n.Frame.H > 0 &&
			(best == nil || n.Frame.W*n.Frame.H > best.W*best.H) {
			best = n.Frame
		}
	}
	if best != nil {
		return *best
	}
	return Frame{X: 0, Y: 0, W: 390, H: 844}
}

// swipeForDirection maps a scroll direction onto swipe coordinates inside
// the region: revealing content below means dragging the finger up.
func swipeForDirection(r Frame, dir string) (x1, y1, x2, y2 float64) {
	cx := r.X + r.W/2
	cy := r.Y + r.H/2
	dy := r.H * scrollSwipeFraction / 2
	dx := r.W * scrollSwipeFraction / 2
	switch dir {
	case "up":
		return cx, cy - dy, cx, cy + dy
	case "left":
		return cx - dx, cy, cx + dx, cy
	case "right":
		return cx + dx, cy, cx - dx, cy
	default: // down
		return cx, cy + dy, cx, cy - dy
	}
}
