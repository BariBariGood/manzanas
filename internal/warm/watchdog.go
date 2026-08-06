package warm

import (
	"context"
	"errors"
	"time"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

const (
	// DefaultFootprintCapKB marks a slim sim unhealthy above ~3 GiB
	// phys_footprint: fleet measurements saw a slim sim balloon
	// 1.9 -> 17.9 GB in 14 minutes, so runaways must be caught early.
	// (A healthy freshly-slimmed sim tree measures ~1 GB.)
	DefaultFootprintCapKB = int64(3 << 20) // 3 GiB in KiB
	// DefaultWatchdogInterval is how often footprints are polled.
	DefaultWatchdogInterval = time.Minute
	// watchdogOpTimeout bounds each destructive watchdog operation
	// (recycle, re-provision): a hung simctl becomes a logged failure
	// instead of blocking every future sweep and holding the target's
	// lease reservation forever.
	watchdogOpTimeout = 10 * time.Minute
	// rebuildBackoffMin/Max bound the per-target backoff after a failed
	// or gate-refused shut-down-member rebuild, so an overloaded host
	// isn't hit with a destructive erase (+ its slim re-apply boot)
	// every single sweep.
	rebuildBackoffMin = 2 * time.Minute
	rebuildBackoffMax = 30 * time.Minute
)

// LeasedFunc reports whether a target currently has an active lease, so
// the watchdog never recycles a sim out from under its holder.
type LeasedFunc func(udid string) bool

// StartWatchdog polls the physical footprint (Host.TreeFootprintKB;
// kernel phys_footprint on macOS) of every pool
// member and recycles runaways: parked/idle members are recycled
// immediately; leased members are only flagged (they get erased at
// release anyway). Returns a stop function.
func (p *Pool) StartWatchdog(ctx context.Context, interval time.Duration, capKB int64, leased LeasedFunc) func() {
	if interval <= 0 {
		interval = DefaultWatchdogInterval
	}
	if capKB <= 0 {
		capKB = DefaultFootprintCapKB
	}
	wctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-t.C:
				p.sweepFootprints(wctx, capKB, leased)
			}
		}
	}()
	return cancel
}

// rebuildDue reports whether the shut-down-member rebuild backoff for
// the target has elapsed.
func (p *Pool) rebuildDue(udid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !time.Now().Before(p.rebuildNext[udid])
}

// deferRebuild pushes the target's next rebuild attempt out, doubling
// the wait per consecutive failure up to rebuildBackoffMax.
func (p *Pool) deferRebuild(udid string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.rebuildWait[udid] * 2
	if d < rebuildBackoffMin {
		d = rebuildBackoffMin
	}
	if d > rebuildBackoffMax {
		d = rebuildBackoffMax
	}
	p.rebuildWait[udid] = d
	p.rebuildNext[udid] = time.Now().Add(d)
	return d
}

func (p *Pool) clearRebuildBackoff(udid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.rebuildWait, udid)
	delete(p.rebuildNext, udid)
}

// sweepFootprints runs one watchdog pass (exported to tests via
// watchdog_test.go; production uses StartWatchdog).
func (p *Pool) sweepFootprints(ctx context.Context, capKB int64, leased LeasedFunc) {
	p.mu.Lock()
	members := make([]string, 0, len(p.members))
	for u := range p.members {
		members = append(members, u)
	}
	p.mu.Unlock()
	if len(members) == 0 {
		// Nothing to watch: don't pay for a whole-machine ps walk.
		return
	}
	procs, err := p.host.Processes(ctx)
	if err != nil {
		p.log.Warn("watchdog: process listing failed", "err", err)
		return
	}
	// stale marks the snapshot outdated after any slow/destructive
	// operation in this loop (recycle, re-provision, park): a member
	// evaluated later must not be judged — or forgotten — against a
	// process table from minutes ago.
	stale := false
	for _, udid := range members {
		if stale {
			procs, err = p.host.Processes(ctx)
			if err != nil {
				p.log.Warn("watchdog: process listing failed", "err", err)
				return
			}
			stale = false
		}
		if p.isBusy(udid) {
			// Mid-add/reset/re-park/recycle: leave it alone.
			continue
		}
		parked := true
		tree := p.prk.ParkedTree(udid)
		if len(tree) > 0 && !anyAlive(procs, tree) {
			// Confirm against a fresh listing before dropping the entry:
			// the tree may have been (re-)parked after the snapshot was
			// taken, and forgetting a live SIGSTOPped tree would strand
			// it frozen past Close's ThawAll.
			fresh, err := p.host.Processes(ctx)
			if err != nil {
				p.log.Warn("watchdog: process listing failed", "err", err)
				return
			}
			procs = fresh
			if !anyAlive(procs, tree) {
				// The parked tree died outside the daemon (crash, external
				// simctl shutdown): drop the stale ledger entry so Health
				// and Boot stop pretending the sim is up.
				p.log.Warn("parked sim tree is gone; dropping stale park entry", "udid", udid)
				p.prk.ForgetParked(udid)
				tree = nil
			} else {
				tree = p.prk.ParkedTree(udid)
			}
		}
		if len(tree) == 0 {
			// Not parked: walk the live tree (leased or mid-cycle).
			parked = false
			tree = simTree(procs, udid)
		}
		if len(tree) == 0 {
			// A member with no tree at all is a shut-down warm sim (e.g. a
			// recycle whose final re-boot was gate-refused): rebuild it so
			// the pool doesn't silently shrink. Sweep interval = backoff.
			// An intentionally shut-down member (SafeShutdown) stays down
			// until an explicit boot or its next lease-end re-park.
			if (leased == nil || !leased(udid)) && !p.isAwake(udid) && !p.isDown(udid) && p.rebuildDue(udid) {
				// Consult the boot gates BEFORE the destructive erase: the
				// rebuild's boot would be refused anyway, and the
				// production erase itself boots the sim to re-apply the
				// slim image — an overloaded host must not pay that every
				// sweep for a rebuild that can't finish.
				if gerr := p.GateBoot(ctx, udid); gerr != nil {
					p.log.Warn("watchdog: member rebuild deferred by boot gates",
						"udid", udid, "retry_in", p.deferRebuild(udid), "err", gerr)
					continue
				}
				stale = true
				func() {
					release, _, ok := p.reserveTarget(udid)
					if !ok {
						return // leased since the check above
					}
					var err error
					defer func() { release(err == nil) }()
					// Runs before the release above (LIFO): a successful
					// rebuild erased the sim, so clear its dirty mark
					// before the hold is dropped.
					defer func() {
						if err == nil {
							p.notifyClean(udid)
						}
					}()
					defer p.BeginTransition(udid)()
					octx, cancel := context.WithTimeout(ctx, watchdogOpTimeout)
					defer cancel()
					// The reservation may have taken over a quarantine
					// (a failed post-lease reset): erase the shut-down
					// sim before rebuilding so the previous holder's
					// data is never released to the next lease.
					if err = p.cfg.Erase(octx, udid); err != nil {
						p.log.Warn("watchdog: member erase failed",
							"udid", udid, "retry_in", p.deferRebuild(udid), "err", err)
						return
					}
					if err = p.rePark(octx, udid); err != nil {
						p.log.Warn("watchdog: member re-provision failed",
							"udid", udid, "retry_in", p.deferRebuild(udid), "err", err)
					} else {
						p.clearRebuildBackoff(udid)
						p.log.Info("watchdog: shut-down member re-provisioned", "udid", udid)
					}
				}()
			}
			continue
		}
		kb, err := p.host.TreeFootprintKB(ctx, tree)
		if err != nil {
			// Fail open: an unmeasurable sim must not be recycled on a
			// guess (summed ps RSS overcounts phys_footprint ~8x and
			// would recycle every healthy slim sim, forever). Treat it
			// as under-cap so the idle re-park below still runs — a
			// persistently broken measurement must not strand idle
			// members awake and burning CPU.
			p.log.Warn("watchdog: footprint measurement failed; treating as under cap", "udid", udid, "err", err)
			kb = 0
		}
		if kb <= capKB {
			// A member left running with no lease (released without a
			// reset spec) goes back to parked so it stops burning CPU —
			// unless it was explicitly booted/thawed (wake grace).
			if !parked && (leased == nil || !leased(udid)) && !p.isAwake(udid) && !p.isDown(udid) {
				// Only park a sim the registry actually reports Booted:
				// a Shutdown-state member can still have a stray
				// launchd_sim tree (external shutdown left orphans), and
				// SIGSTOPping it would make Health report Booted forever
				// while Boot merely thaws — advertised up but unusable.
				// Left alone, the tree dies or is reaped and the rebuild
				// branch takes over on a later sweep.
				st, herr := p.reg.Health(ctx, udid)
				var nf *registry.NotFoundError
				if errors.As(herr, &nf) {
					// The sim no longer exists: conclusive, not
					// transient — parking its leftover tree would
					// advertise a deleted device as Booted forever.
					p.log.Warn("watchdog: idle member no longer exists; skipping re-park",
						"udid", udid, "err", herr)
					continue
				}
				if herr != nil {
					// Fail open, like the footprint check above: an
					// unreadable state is not evidence the sim is down,
					// and a persistently broken query must not strand
					// idle members awake burning CPU.
					p.log.Warn("watchdog: idle member health check failed; re-parking anyway",
						"udid", udid, "err", herr)
				} else if st != proto.StateBooted {
					p.log.Info("watchdog: idle member not Booted; skipping re-park",
						"udid", udid, "state", st)
					continue
				}
				stale = true
				if ok, err := p.parkIdle(ctx, udid); err != nil {
					p.log.Warn("watchdog: idle member re-park failed", "udid", udid, "err", err)
				} else if ok {
					p.log.Info("watchdog: idle member re-parked", "udid", udid)
				}
			}
			continue
		}
		if leased != nil && leased(udid) {
			p.log.Warn("watchdog: leased sim over footprint cap; will recycle at release",
				"udid", udid, "footprint_kb", kb, "cap_kb", capKB)
			continue
		}
		p.log.Warn("watchdog: sim over footprint cap; recycling",
			"udid", udid, "footprint_kb", kb, "cap_kb", capKB)
		stale = true
		func() {
			octx, cancel := context.WithTimeout(ctx, watchdogOpTimeout)
			defer cancel()
			if err := p.Recycle(octx, udid); err != nil {
				p.log.Error("watchdog: recycle failed", "udid", udid, "err", err)
			}
		}()
	}
}
