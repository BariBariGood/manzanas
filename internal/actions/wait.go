package actions

import (
	"context"
	"errors"
	"time"
)

// Wait defaults and bounds shared by the wait_* actions. Callers can tune
// timeout_ms / interval_ms per request; both are clamped so a bad payload
// cannot hold an action slot forever or hot-spin the a11y bridge.
const (
	defaultWaitTimeout  = 10 * time.Second
	defaultWaitInterval = 500 * time.Millisecond
	maxWaitTimeout      = 2 * time.Minute
	minWaitInterval     = 10 * time.Millisecond
)

// errNotYet is returned by a poll function to mean "keep polling".
var errNotYet = errors.New("not yet")

// waitParams reads the shared timeout_ms / interval_ms payload fields.
func waitParams(p map[string]any, defTimeout time.Duration) (timeout, interval time.Duration, err error) {
	timeout, err = optDuration(p, "timeout_ms", defTimeout)
	if err != nil {
		return 0, 0, err
	}
	if timeout > maxWaitTimeout {
		timeout = maxWaitTimeout
	}
	interval, err = optDuration(p, "interval_ms", defaultWaitInterval)
	if err != nil {
		return 0, 0, err
	}
	if interval < minWaitInterval {
		interval = minWaitInterval
	}
	return timeout, interval, nil
}

// optDuration reads an optional positive millisecond payload field.
func optDuration(p map[string]any, key string, def time.Duration) (time.Duration, error) {
	v, ok := p[key]
	if !ok {
		return def, nil
	}
	n, err := toNum(v)
	if err != nil || n <= 0 {
		return 0, badRequest("payload field %q must be a positive number of milliseconds", key)
	}
	return time.Duration(n) * time.Millisecond, nil
}

// pollUntil runs fn every interval until it succeeds, fails hard, or the
// timeout budget is exhausted. fn returns errNotYet to keep polling. The
// first poll runs immediately. Each poll is bounded by the remaining
// budget, so one slow poll cannot run the wait far past its timeout; a
// poll that fails because that budget expired counts as a timeout, not a
// hard failure. Returns the poll count and elapsed time.
func pollUntil(ctx context.Context, timeout, interval time.Duration, fn func(context.Context) error) (polls int, elapsed time.Duration, err error) {
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		polls++
		pctx, cancel := context.WithDeadline(ctx, deadline)
		err = fn(pctx)
		cancel()
		elapsed = time.Since(start)
		if err != nil && !errors.Is(err, errNotYet) && ctx.Err() == nil &&
			(errors.Is(err, context.DeadlineExceeded) || time.Now().After(deadline)) {
			return polls, elapsed, errNotYet
		}
		if err == nil || !errors.Is(err, errNotYet) {
			return polls, elapsed, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return polls, elapsed, errNotYet
		}
		wait := interval
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return polls, time.Since(start), ctx.Err()
		case <-time.After(wait):
		}
	}
}

// observeOnce runs a single describe-ui poll and compacts it. Transient
// bridge races (not attached yet, non-JSON output) come back as errNotYet
// so wait loops treat them as "no tree yet" instead of failing. refresh
// skips the resident warm helper so the poll always runs on a freshly
// spawned AXe process, whose accessibility connection cannot be stale.
func (b *AXeBackend) observeOnce(ctx context.Context, udid string, refresh bool) (observation, error) {
	// A refresh can only be honored when the cold CLI exists; on a
	// helper-only host the warm read is the sole way to observe.
	if b.warmObserve != nil && (!refresh || b.axePath == "") {
		obs, err := b.warmObserve(ctx, udid)
		var te *TransportError
		if !errors.As(err, &te) {
			return obs, err
		}
		// Warm helper unavailable: run this poll cold.
	}
	raw, err := b.axe(ctx, udid, "describe-ui")
	if err != nil {
		if isTransientA11yError(err) {
			return observation{}, errNotYet
		}
		return observation{}, err
	}
	nodes, err := CompactTree(raw)
	if err != nil || len(nodes) == 0 {
		// A skeleton tree that compacts to nothing is the same
		// bridge-not-ready race as a non-JSON body; keep polling. The
		// wait's own timeout bounds a legitimately element-free screen.
		return observation{}, errNotYet
	}
	return observation{nodes: nodes, viewport: rawViewport(raw)}, nil
}
