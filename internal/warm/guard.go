package warm

import (
	"context"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// GuardedRegistry wraps a registry so the pool's safety rules hold on
// every path:
//   - Boot on a parked pool sim is a thaw (fast path — no gates: the sim
//     is already booted, just SIGSTOPped; `simctl spawn`/AXe against a
//     parked sim would hang otherwise).
//   - Any real boot passes the load gate, free-space gate, running-sim
//     cap, and the class's boot-concurrency limit.
//   - Shutdown always thaws first (shutdown-while-parked wedges ~34s)
//     and reaps orphaned launchd_sim trees afterwards.
type GuardedRegistry struct {
	registry.Registry
	pool *Pool
}

// Guard wraps reg with the pool's gates.
func Guard(reg registry.Registry, pool *Pool) *GuardedRegistry {
	return &GuardedRegistry{Registry: reg, pool: pool}
}

// Boot preserves the registry contract that boots are asynchronous:
// gate refusals return immediately, the boot itself runs in the
// background and callers poll for state.
func (g *GuardedRegistry) Boot(ctx context.Context, udid string) error {
	if g.pool.Parker().IsParked(udid) {
		if err := g.pool.Parker().Thaw(udid); err != nil {
			return err
		}
		// An accepted explicit boot cancels an intentional shutdown:
		// the member goes back under the watchdog's care.
		g.pool.clearDown(udid)
		// A parked sim is demonstrably up: any recorded background
		// boot failure is obsolete.
		g.pool.clearBootErr(udid)
		// An explicit boot must stick for a while: without the grace
		// marker the watchdog would re-park the un-leased member on its
		// next sweep, silently undoing the boot.
		g.pool.MarkAwake(udid)
		return nil
	}
	if err := g.pool.BootAsync(ctx, udid); err != nil {
		return err
	}
	// Only an accepted boot cancels an intentional shutdown and earns
	// the wake grace: a gate-refused (or otherwise rejected) request
	// must not hand a deliberately shut-down member back to the
	// watchdog's rebuild, nor disable its re-park, for a boot that
	// never happened.
	// The wake grace applies to non-members too: a leaseless operator
	// boot (dash) must not be reclaimed by the janitor moments later.
	// A lease ending on the sim clears the grace (OnLeaseEnd), so
	// lease-driven boots are still reclaimed promptly.
	if g.pool.IsMember(udid) {
		g.pool.clearDown(udid)
	}
	g.pool.MarkAwake(udid)
	return nil
}

func (g *GuardedRegistry) Shutdown(ctx context.Context, udid string) error {
	return g.pool.SafeShutdown(ctx, udid)
}

// Health reports a parked pool sim as Booted without touching simctl
// (the underlying query is accurate anyway — SIGSTOP doesn't change the
// CoreSimulator state — but skipping the shell-out keeps parked sims
// cheap to poll).
func (g *GuardedRegistry) Health(ctx context.Context, udid string) (proto.TargetState, error) {
	if g.pool.Parker().IsParked(udid) {
		return proto.StateBooted, nil
	}
	return g.Registry.Health(ctx, udid)
}
