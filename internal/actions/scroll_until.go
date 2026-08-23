package actions

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"
)

// scroll_until: gesture down a list until a text run is visible, with
// movement detection so a bounce-back overscroll is not mistaken for
// progress. Each iteration is gesture → settle (bounded poll until the
// masked text set holds still) → moved check (Jaccard overlap of the
// masked before/after text sets under the threshold) → match check.
// End-of-list is two consecutive non-moving gestures, each confirmed by
// a lazy-load grace re-poll so a spinner that resolves into new rows is
// not read as the end. "Not found" is a result ({found: false}), not an
// error: the caller learns how far the list went and why the loop
// stopped (end_of_list vs max_scrolls) instead of a bare timeout.

const (
	defaultScrollUntilTimeout = 30 * time.Second
	// defaultMovedJaccard splits real scroll progress (well under it)
	// from a bounce-back overscroll (well over it); tuned on the
	// phone-mirror OCR harness, payload-tunable via moved_jaccard.
	defaultMovedJaccard = 0.6
	// settleSamples consecutive identical masked text sets count the
	// screen as settled after a gesture.
	settleSamples = 2
	// settleBudget bounds the per-gesture settle poll so a continuously
	// animating screen cannot eat the whole action budget.
	settleBudget = 3 * time.Second
	// endOfListNonMoves consecutive confirmed non-moves mean the list
	// has no more content in that direction.
	endOfListNonMoves = 2
)

// listDriver is what the scrolling composites need from a backend: the
// shared observe/tap/type primitives plus the per-backend list gesture.
type listDriver interface {
	elementDriver
	listGestureDriver
}

// listScrollParams are the payload fields shared by scroll_until and
// scroll_collect.
type listScrollParams struct {
	direction    string
	maxScrolls   int
	movedJaccard float64
	refresh      bool
	timeout      time.Duration
	interval     time.Duration
}

func listScrollParamsFromPayload(p map[string]any) (listScrollParams, error) {
	var lp listScrollParams
	var err error
	lp.direction, err = scrollDirection(p)
	if err != nil {
		return lp, err
	}
	lp.maxScrolls = defaultMaxScrolls
	if v, ok := p["max_scrolls"]; ok {
		n, err := toNum(v)
		if err != nil || n < 1 || n > maxMaxScrolls {
			return lp, badRequest("payload field %q must be a number between 1 and %d", "max_scrolls", maxMaxScrolls)
		}
		lp.maxScrolls = int(n)
	}
	lp.movedJaccard = defaultMovedJaccard
	if v, ok := p["moved_jaccard"]; ok {
		n, err := toNum(v)
		if err != nil || n <= 0 || n > 1 {
			return lp, badRequest("payload field %q must be a number in (0, 1]", "moved_jaccard")
		}
		lp.movedJaccard = n
	}
	lp.refresh, err = boolFlag(p, "refresh", false)
	if err != nil {
		return lp, err
	}
	lp.timeout, lp.interval, err = waitParams(p, defaultScrollUntilTimeout)
	if err != nil {
		return lp, err
	}
	return lp, nil
}

// listScrollResult is what one gesture loop produced, shaped into the
// shared result fields by the callers.
type listScrollResult struct {
	scrolls      int
	screens      int
	movedHistory []float64
	endOfList    bool
	stopped      bool
	lastObs      observation
	elapsed      time.Duration
}

func (r listScrollResult) fields() map[string]any {
	history := r.movedHistory
	if history == nil {
		history = []float64{}
	}
	return map[string]any{
		"scrolls":       r.scrolls,
		"screens":       r.screens,
		"moved_history": history,
		"end_of_list":   r.endOfList,
		"elapsed_ms":    r.elapsed.Milliseconds(),
		"hash":          TreeHash(r.lastObs.nodes),
	}
}

// listScrollLoop drives the gesture → settle → moved-check loop, calling
// visit on every settled screen (the pre-gesture screen first). visit
// returning true stops the loop (the target matched / harvest is done).
func listScrollLoop(ctx context.Context, d listDriver, udid string, lp listScrollParams, visit func(obs observation) bool) (listScrollResult, error) {
	var res listScrollResult
	start := time.Now()
	deadline := start.Add(lp.timeout)
	obs, err := observeReadable(ctx, d, udid, lp.refresh, deadline, lp.interval)
	if err != nil {
		if errors.Is(err, errNotYet) || errors.Is(err, context.DeadlineExceeded) {
			return res, timeoutErr("no readable tree within the %s budget (%s elapsed); the screen never produced an observation to scroll from", lp.timeout, time.Since(start).Round(time.Millisecond))
		}
		return res, err
	}
	res.screens = 1
	res.lastObs = obs
	res.elapsed = time.Since(start)
	if visit(obs) {
		res.stopped = true
		return res, nil
	}
	prevSet := textSet(obs.nodes, obs.viewport)
	nonMoves := 0
	for res.scrolls < lp.maxScrolls && time.Now().Before(deadline) {
		region := scrollRegion(obs)
		// A gesture the helper delivered but pixel-verified as no-change
		// (errGestureNoEffect) is how the end of a list looks under
		// per-action verification, so it is not fatal. The verdict is
		// not trusted blindly either — the helper can miss a late render
		// — so the loop falls through to the ordinary settle + Jaccard
		// moved check and lets that accounting decide.
		if err := d.scrollGesture(ctx, udid, region, lp.direction); err != nil &&
			!errors.Is(err, errGestureNoEffect) {
			return res, err
		}
		res.scrolls++
		obs, err = settleObservation(ctx, d, udid, lp.refresh, deadline, lp.interval)
		if err != nil {
			if errors.Is(err, errNotYet) || errors.Is(err, context.DeadlineExceeded) {
				// The overall budget expiring mid-settle is a soft loop
				// exit: everything harvested so far stays in the result.
				// The hard timeout is reserved for a screen that stopped
				// producing observations while budget remained.
				if !time.Now().Before(deadline) {
					break
				}
				return res, timeoutErr("no readable tree after gesture %d (%s elapsed); the screen stopped producing observations mid-scroll", res.scrolls, time.Since(start).Round(time.Millisecond))
			}
			return res, err
		}
		newSet := textSet(obs.nodes, obs.viewport)
		j := jaccard(prevSet, newSet)
		if j >= lp.movedJaccard && time.Now().Before(deadline) {
			// Lazy-load grace: a list that paused on a spinner looks
			// unmoved for a beat, then resolves into new rows. One
			// re-poll after an interval before the non-move counts.
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(lp.interval):
			}
			if o2, err2 := d.observeOnce(ctx, udid, lp.refresh); err2 == nil {
				if s2 := textSet(o2.nodes, o2.viewport); jaccard(prevSet, s2) < j {
					obs, newSet = o2, s2
					j = jaccard(prevSet, newSet)
				}
			}
		}
		moved := j < lp.movedJaccard
		res.movedHistory = append(res.movedHistory, math.Round(j*1000)/1000)
		res.screens++
		res.lastObs = obs
		res.elapsed = time.Since(start)
		if visit(obs) {
			res.stopped = true
			return res, nil
		}
		if moved {
			nonMoves = 0
		} else {
			nonMoves++
			if nonMoves >= endOfListNonMoves {
				res.endOfList = true
				break
			}
		}
		prevSet = newSet
	}
	res.elapsed = time.Since(start)
	return res, nil
}

// settleObservation polls until the masked text set holds still for
// settleSamples consecutive polls, bounded by settleBudget (and the
// caller's deadline). A screen still animating when the budget runs out
// is not an error — the last readable observation is returned, and the
// moved check absorbs the residual jitter.
func settleObservation(ctx context.Context, d elementDriver, udid string, refresh bool, deadline time.Time, interval time.Duration) (observation, error) {
	settleBy := time.Now().Add(settleBudget)
	if settleBy.After(deadline) {
		settleBy = deadline
	}
	var last observation
	var lastSet map[string]struct{}
	haveLast := false
	streak := 0
	_, _, err := pollUntil(ctx, time.Until(settleBy), interval, func(ctx context.Context) error {
		obs, err := d.observeOnce(ctx, udid, refresh)
		if err != nil {
			if errors.Is(err, errNotYet) {
				streak = 0
			}
			return err
		}
		set := textSet(obs.nodes, obs.viewport)
		if haveLast && jaccard(set, lastSet) == 1 {
			streak++
		} else {
			streak = 1
		}
		last, lastSet, haveLast = obs, set, true
		if streak >= settleSamples {
			return nil
		}
		return errNotYet
	})
	if err != nil && haveLast &&
		(errors.Is(err, errNotYet) || errors.Is(err, context.DeadlineExceeded)) {
		return last, nil
	}
	if err != nil {
		return observation{}, err
	}
	return last, nil
}

// findTextRun DFS-scans the tree for a node whose visible text contains
// needle (case-insensitive). Label, value, and placeholder all count —
// on OCR trees only labels exist, on a11y trees the target text can sit
// in any of the three. A node with a frame must sit inside the viewport:
// a11y trees carry off-screen content, and "until" means visible.
func findTextRun(nodes []*Node, viewport *Frame, needle string) *Node {
	for _, n := range nodes {
		if n == nil {
			continue
		}
		visible := true
		if n.Frame != nil && viewport != nil {
			cx, cy := n.Frame.Center()
			visible = pointInViewport(cx, cy, viewport)
		}
		if visible {
			for _, s := range []string{n.Label, n.Value, n.Placeholder} {
				if s != "" && strings.Contains(strings.ToLower(s), needle) {
					return n
				}
			}
		}
		if hit := findTextRun(n.Children, viewport, needle); hit != nil {
			return hit
		}
	}
	return nil
}

func elemScrollUntil(ctx context.Context, d listDriver, udid string, p map[string]any) (map[string]any, error) {
	text, ok := p["text"].(string)
	if !ok || strings.TrimSpace(text) == "" {
		return nil, badRequest("scroll_until requires a non-empty string payload field %q (v1 matches visible text only)", "text")
	}
	lp, err := listScrollParamsFromPayload(p)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(strings.TrimSpace(text))
	var found *Node
	res, err := listScrollLoop(ctx, d, udid, lp, func(obs observation) bool {
		found = findTextRun(obs.nodes, obs.viewport, needle)
		return found != nil
	})
	if err != nil {
		return nil, err
	}
	out := res.fields()
	out["found"] = res.stopped
	if found != nil {
		out["element"] = leafCopy(found)
	}
	return out, nil
}

func handleScrollUntil(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	return elemScrollUntil(ctx, b, udid, p)
}

func handleDeviceScrollUntil(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	if _, err := b.wdaFor(udid); err != nil {
		return nil, err
	}
	return elemScrollUntil(ctx, b, udid, p)
}

func handleMirrorScrollUntil(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	return withMirrorFidelity(elemScrollUntil(ctx, b, udid, p))
}
