package actions

import (
	"context"
	"errors"
	"time"
)

// Stability defaults: the tree must hash identically for stableSamples
// consecutive polls (i.e. hold still for (stableSamples-1) intervals)
// before the UI counts as settled.
const (
	defaultStableSamples = 3
	maxStableSamples     = 20
	defaultStableTimeout = 15 * time.Second
)

// handleWaitTreeStable polls the a11y tree until its content hash is
// unchanged for N consecutive samples, meaning animations and loads have
// settled. It returns the final hash and how long settling took. A tree
// that never holds still within the budget (e.g. a continuously-animating
// screen) is not an error: the max-wait elapses and the result carries
// stable:false, so callers can distinguish "the UI is live" from a
// transport/poll failure. Prefer wait_for_element on such screens.
func handleWaitTreeStable(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	return elemTreeStable(ctx, b, udid, p)
}

func elemTreeStable(ctx context.Context, d elementDriver, udid string, p map[string]any) (map[string]any, error) {
	need := defaultStableSamples
	if v, ok := p["stable_samples"]; ok {
		n, err := toNum(v)
		if err != nil || n < 2 || n > maxStableSamples {
			return nil, badRequest("payload field %q must be a number between 2 and %d", "stable_samples", maxStableSamples)
		}
		need = int(n)
	}
	// Callers that treat "never settled" as a hard failure (e.g. an eval
	// gating a deterministic hash capture) opt in; the default is the
	// soft stable:false result.
	requireStable, err := boolFlag(p, "require_stable", false)
	if err != nil {
		return nil, err
	}
	refresh, err := boolFlag(p, "refresh", false)
	if err != nil {
		return nil, err
	}
	timeout, interval, err := waitParams(p, defaultStableTimeout)
	if err != nil {
		return nil, err
	}

	// lastHash tracks the streak and is cleared on transient read failures;
	// lastGoodHash survives them and records whether any poll ever produced
	// a readable tree.
	var lastHash, lastGoodHash string
	streak := 0
	polls, elapsed, err := pollUntil(ctx, timeout, interval, func(ctx context.Context) error {
		obs, err := d.observeOnce(ctx, udid, refresh)
		if err != nil {
			if errors.Is(err, errNotYet) {
				streak, lastHash = 0, ""
			}
			return err
		}
		h := TreeHash(obs.nodes)
		lastGoodHash = h
		if h == lastHash {
			streak++
		} else {
			lastHash, streak = h, 1
		}
		if streak >= need {
			return nil
		}
		return errNotYet
	})
	if err != nil {
		// The caller's own deadline (e.g. the batch wall-clock budget)
		// cutting the wait short is a real timeout, not an observation
		// that the UI is live: the wait never spent its budget.
		if ctx.Err() != nil {
			return nil, timeoutErr("wait_tree_stable ended early after %s (%d poll(s)): %v", elapsed.Round(time.Millisecond), polls, ctx.Err())
		}
		if errors.Is(err, errNotYet) || errors.Is(err, context.DeadlineExceeded) {
			// Distinct "unstable" outcome: the max-wait elapsed with the
			// tree still changing (a continuously-animating screen can
			// never satisfy the streak). This is an observation, not a
			// failure, so it is reported as a successful result rather
			// than a generic timeout error.
			if requireStable {
				return nil, timeoutErr("tree did not stabilize within the %s max wait (%d poll(s), %s elapsed, last hash %s)", timeout, polls, elapsed.Round(time.Millisecond), lastGoodHash)
			}
			if lastGoodHash == "" {
				// No poll ever produced a readable tree (a11y bridge never
				// attached, or every read failed transiently): that is a
				// real failure, not a live UI.
				return nil, timeoutErr("no readable a11y tree within the %s max wait (%d poll(s), %s elapsed)", timeout, polls, elapsed.Round(time.Millisecond))
			}
			return map[string]any{
				"stable":     false,
				"hash":       lastGoodHash,
				"settled_ms": elapsed.Milliseconds(),
				"samples":    polls,
			}, nil
		}
		return nil, err
	}
	return map[string]any{
		"stable":     true,
		"hash":       lastHash,
		"settled_ms": elapsed.Milliseconds(),
		"samples":    polls,
	}, nil
}
