package warm

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

const (
	// DefaultJanitorInterval is how often the janitor reconciles booted
	// simulators against the pool and the lease table.
	DefaultJanitorInterval = time.Minute
	// janitorIdleGrace is how long a daemon-booted, non-pool sim must
	// stay idle (no lease, stream, or recording) before the janitor
	// shuts it down: a holder that releases and immediately re-acquires
	// must not have the sim yanked between its leases.
	janitorIdleGrace = 90 * time.Second
	// janitorOpTimeout bounds each janitor shutdown so a hung simctl
	// becomes a logged failure instead of blocking every future sweep.
	janitorOpTimeout = 5 * time.Minute
)

// markDaemonBooted records that this daemon booted the sim (via the
// gated client boot path) so the janitor may later shut it down when it
// goes idle. Pool members are excluded: their lifecycle (re-park,
// recycle, rebuild) is owned by the watchdog.
func (p *Pool) markDaemonBooted(udid string) {
	p.mu.Lock()
	if !p.members[udid] {
		p.daemonBooted[udid] = true
	}
	p.mu.Unlock()
}

// forgetDaemonBooted drops the boot-ownership record (after the sim is
// shut down, by any path).
func (p *Pool) forgetDaemonBooted(udid string) {
	p.mu.Lock()
	delete(p.daemonBooted, udid)
	// The idle mark goes with it: a stale mark surviving here would let a
	// later boot of the same UDID be reclaimed without a fresh grace.
	delete(p.idleSince, udid)
	p.mu.Unlock()
}

// IsDaemonBooted reports whether this daemon booted the sim outside the
// pool (and has not shut it down since).
func (p *Pool) IsDaemonBooted(udid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.daemonBooted[udid]
}

// daemonBootedList snapshots the boot-ownership ledger.
func (p *Pool) daemonBootedList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.daemonBooted))
	for u := range p.daemonBooted {
		out = append(out, u)
	}
	return out
}

// OnLeaseEnd reclaims a daemon-booted, non-pool sim the moment its lease
// reaches a terminal state with no reset in flight: without this the sim
// would stay Booted (burning CPU) until an operator noticed. Pool members
// are left to the reset/watchdog machinery; sims the daemon never booted
// (an agent's own simctl work) are never touched. Run it off the lease
// manager's event goroutine — the shutdown shells out to simctl.
func (p *Pool) OnLeaseEnd(udid string) {
	if udid == "" || p.IsMember(udid) {
		return
	}
	// The boot behind the ended lease earned a wake grace (the guarded
	// boot path marks every accepted boot awake); the lease lifecycle
	// owned that boot, so the grace ends with the lease. Cleared before
	// the ledger check: a lease ending mid-boot precedes the ownership
	// record (written when the boot completes), and leaving the grace
	// would shield the sim from the sweep for the full 15 minutes.
	p.clearAwake(udid)
	if !p.IsDaemonBooted(udid) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), janitorOpTimeout)
	defer cancel()
	if p.reclaimIdle(ctx, udid) {
		p.log.Info("lease ended; reclaimed daemon-booted sim", "udid", udid)
	}
}

// reclaimIdle shuts a daemon-booted, non-pool sim down if it is still
// idle: not leased, not mid-transition, not streaming, not recording,
// and not under an explicit-boot wake grace. The lease-manager hold
// guarantees no lease can be granted mid-shutdown. Reports whether the
// sim was shut down.
func (p *Pool) reclaimIdle(ctx context.Context, udid string) bool {
	// The membership check runs here (not just in the callers): a sim can
	// join the pool after it entered the daemon-booted ledger (a client
	// boot racing startup provisioning), and a member must never be
	// reclaimed — the watchdog owns its lifecycle.
	if p.IsMember(udid) || p.isLeased(udid) || p.isBusy(udid) || p.isAwake(udid) ||
		p.isStreaming(udid) || p.isRecording(udid) {
		return false
	}
	st, err := p.reg.Health(ctx, udid)
	var nf *registry.NotFoundError
	if errors.As(err, &nf) {
		// The sim was deleted: nothing to shut down, drop the record.
		p.forgetDaemonBooted(udid)
		return false
	}
	if err != nil || st != proto.StateBooted {
		// Only a confirmed Shutdown drops the record: a sim still
		// Booting (a boot that outlived its poll) keeps its owner so a
		// later sweep can reclaim it once it comes up.
		if err == nil && st == proto.StateShutdown {
			p.forgetDaemonBooted(udid)
		}
		return false
	}
	release, takeover, ok := p.reserveTarget(udid)
	if !ok {
		return false // leased since the checks above
	}
	// A reclaim never erases, so a displaced failed-reset quarantine
	// must stay quarantined; a plain free target goes back to leasable
	// either way (shut down or untouched).
	defer func() { release(!takeover) }()
	if p.isStreaming(udid) || p.isRecording(udid) || p.isBusy(udid) {
		return false
	}
	if err := p.SafeShutdown(ctx, udid); err != nil {
		p.log.Warn("janitor: reclaim shutdown failed", "udid", udid, "err", err)
		return false
	}
	p.reportShutdown(udid, "janitor", "idle daemon-booted sim reclaimed (no lease, stream, or recording)")
	p.forgetDaemonBooted(udid)
	return true
}

// StartJanitor runs the periodic reconciler: every interval it sweeps
// daemon-booted, non-pool sims and shuts down any that have stayed idle
// (no lease/stream/recording) for at least janitorIdleGrace, and logs
// Booted sims it does not manage (pool members and daemon-booted sims
// aside) so operators can see them without the janitor ever touching
// them. Returns a stop function.
func (p *Pool) StartJanitor(ctx context.Context, interval time.Duration) func() {
	if interval <= 0 {
		interval = DefaultJanitorInterval
	}
	jctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-jctx.Done():
				return
			case <-t.C:
				p.sweepJanitor(jctx)
			}
		}
	}()
	return cancel
}

// sweepJanitor runs one janitor pass (exported to tests; production uses
// StartJanitor).
func (p *Pool) sweepJanitor(ctx context.Context) {
	now := time.Now()
	for _, udid := range p.daemonBootedList() {
		if p.isLeased(udid) || p.isBusy(udid) || p.isAwake(udid) ||
			p.isStreaming(udid) || p.isRecording(udid) {
			p.clearIdleSince(udid)
			continue
		}
		since := p.idleSinceOrMark(udid, now)
		if now.Sub(since) < janitorIdleGrace {
			continue
		}
		octx, cancel := context.WithTimeout(ctx, janitorOpTimeout)
		reclaimed := p.reclaimIdle(octx, udid)
		cancel()
		if reclaimed {
			p.clearIdleSince(udid)
			p.log.Info("janitor: reclaimed idle daemon-booted sim", "udid", udid)
		}
	}
	unmanaged, err := p.unmanagedBooted(ctx)
	if err != nil {
		p.log.Warn("janitor: unmanaged-sim listing failed", "err", err)
		p.resetReapMarks()
		return
	}
	p.logUnmanaged(unmanaged)
	if p.cfg.ReapStaleLocks {
		p.reapStaleLocked(ctx, unmanaged, now)
	}
}

// idleSinceOrMark returns when the sim was first observed idle, marking
// it now if this is the first idle observation.
func (p *Pool) idleSinceOrMark(udid string, now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.idleSince[udid]; ok {
		return t
	}
	p.idleSince[udid] = now
	return now
}

func (p *Pool) clearIdleSince(udid string) {
	p.mu.Lock()
	delete(p.idleSince, udid)
	p.mu.Unlock()
}

// unmanagedBooted lists Booted, un-parked simulators the daemon neither
// pools nor booted itself (e.g. an agent's own simctl-created sims).
// The janitor only touches these when stale-lock reaping is opted in
// (Config.ReapStaleLocks) and the sim's fleet lock is stale or missing;
// otherwise they are only surfaced for operators.
func (p *Pool) unmanagedBooted(ctx context.Context) ([]proto.Target, error) {
	targets, err := p.reg.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []proto.Target
	p.mu.Lock()
	for _, t := range targets {
		if t.Kind != proto.TargetSimulator || t.State != proto.StateBooted {
			continue
		}
		if p.members[t.UDID] || p.daemonBooted[t.UDID] || p.prk.IsParked(t.UDID) {
			continue
		}
		out = append(out, t)
	}
	p.mu.Unlock()
	return out, nil
}

// logUnmanaged logs Booted sims outside the daemon's management when the
// set changes, so a forgotten agent sim shows up in the daemon log
// without spamming every sweep.
func (p *Pool) logUnmanaged(unmanaged []proto.Target) {
	// The key is order-independent: the registry lists sims in map order,
	// so an unsorted key would look changed on every sweep.
	udids := make([]string, 0, len(unmanaged))
	for _, t := range unmanaged {
		udids = append(udids, t.UDID)
	}
	sort.Strings(udids)
	key := strings.Join(udids, ",")
	p.mu.Lock()
	changed := key != p.lastUnmanaged
	p.lastUnmanaged = key
	p.mu.Unlock()
	if !changed {
		return
	}
	if len(unmanaged) == 0 {
		p.log.Info("janitor: no unmanaged booted sims")
		return
	}
	for _, t := range unmanaged {
		p.log.Info("janitor: unmanaged booted sim",
			"udid", t.UDID, "name", t.Name, "reap_stale_locks", p.cfg.ReapStaleLocks)
	}
}
