package actions

import (
	"context"
	"fmt"
	"time"
)

// wdaAXResolver is the physical-device rung 1: one WDA /source snapshot,
// compacted to the shared Node tree. Failure classification matches
// observeOnce's: transient transport failures (a tunnel blip, a runner
// mid-relaunch) keep the wait polling — via errAXSourceDown, so the
// ladder can still charge the breaker if the source never comes back
// within the step — while protocol-level failures are hard rung
// failures.
type wdaAXResolver struct {
	b *DeviceBackend
}

func (r *wdaAXResolver) name() string { return resolverAX }

// budget is wider than the simulator rung's: a WDA /source snapshot of
// a heavy screen legitimately takes seconds, and the clamp only needs
// to beat the WDA client's 30s round-trip so a hung snapshot trips the
// breaker instead of eating the wait budget.
func (r *wdaAXResolver) budget() time.Duration { return wdaAXRungBudget }

func (r *wdaAXResolver) resolve(ctx context.Context, udid string, m elementMatcher, refresh bool) (*Node, observation, error) {
	c, err := r.b.wdaFor(udid)
	if err != nil {
		// No endpoint configured: a setup problem the breaker must not
		// count — keep the actionable message.
		return nil, observation{}, fmt.Errorf("%w: %w", errResolverConfig, err)
	}
	src, err := c.Source(ctx)
	if err != nil {
		if ctx.Err() != nil {
			// The rung budget cut off a slow (not necessarily dead)
			// snapshot: report the timeout without kicking the
			// supervisor over it.
			return nil, observation{}, ctx.Err()
		}
		if isTransientWDAError(err) {
			// Kick the supervisor and let the wait keep polling; the
			// blip heals within the budget exactly like observeOnce.
			r.b.wdaFail(udid, "observe", err)
			return nil, observation{}, errAXSourceDown
		}
		return nil, observation{}, r.b.wdaFail(udid, "observe", err)
	}
	nodes, err := CompactWDATree(src)
	if err != nil || len(nodes) == 0 {
		return nil, observation{}, errNotYet
	}
	return matchIn(observation{nodes: nodes, viewport: wdaViewport(src)}, m)
}
