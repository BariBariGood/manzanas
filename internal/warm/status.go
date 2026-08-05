package warm

import (
	"context"
	"time"
)

// runningCacheTTL bounds how stale the cached running count served by
// Status may be. runningCount shells out to simctl via the registry, so
// status probes must not trigger a shell-out per request.
const runningCacheTTL = 15 * time.Second

// PoolStatus is the pool's contribution to the daemon's GET /v0/status
// payload: capacity, occupancy, and the safety gates evaluated with the
// current thresholds.
type PoolStatus struct {
	Class   CapacityClass
	Running int
	// Unmanaged counts Booted, un-parked simulators the daemon neither
	// pools nor booted itself (another agent's sims); the janitor never
	// touches them, but schedulers/operators can see them here.
	Unmanaged int
	// ReapedStaleLocks counts unmanaged sims the janitor has shut down
	// because their fleet lock was stale or missing (only ever non-zero
	// when Config.ReapStaleLocks is on).
	ReapedStaleLocks int
	Parked           int
	BootsInFlight    int
	LoadAvg1         float64
	CPUs             int
	FreeDiskBytes    uint64
	LoadOK           bool
	DiskOK           bool
}

// Running returns the count of Booted, un-parked sims, served from a
// short-lived cache (refreshed at most every runningCacheTTL) so status
// probes don't shell out to simctl each time.
func (p *Pool) Running(ctx context.Context) (int, error) {
	p.mu.Lock()
	if !p.runningAt.IsZero() && time.Since(p.runningAt) < runningCacheTTL {
		n := p.running
		p.mu.Unlock()
		return n, nil
	}
	p.mu.Unlock()
	n, err := p.runningCount(ctx, "")
	if err != nil {
		return 0, err
	}
	return n, nil
}

// BootsInFlight returns how many boot slots are currently held.
func (p *Pool) BootsInFlight() int { return len(p.bootSlots) }

// Status snapshots the pool for the daemon's /v0/status endpoint. Gauge
// failures fail open (gate reported ok), matching GateBoot's behavior.
func (p *Pool) Status(ctx context.Context) PoolStatus {
	st := PoolStatus{
		Class:         p.cfg.Class,
		Parked:        p.prk.ParkedCount(),
		BootsInFlight: p.BootsInFlight(),
		CPUs:          p.host.NumCPU(),
		LoadOK:        true,
		DiskOK:        true,
	}
	if n, err := p.Running(ctx); err == nil {
		st.Running = n
		p.mu.Lock()
		st.Unmanaged = p.unmanagedN
		p.mu.Unlock()
	}
	p.mu.Lock()
	st.ReapedStaleLocks = p.reapedN
	p.mu.Unlock()
	// Gates are derived from the samples just taken (same thresholds as
	// checkLoad/checkDisk): re-measuring doubles the shell-outs and lets
	// the reported gauge disagree with the reported gate.
	if load, err := p.host.LoadAvg1(); err == nil {
		st.LoadAvg1 = load
		if p.cfg.LoadFactor > 0 && load > p.cfg.LoadFactor*float64(st.CPUs) {
			st.LoadOK = false
		}
	}
	if free, err := p.host.FreeDiskBytes(p.cfg.DevicesDir); err == nil {
		st.FreeDiskBytes = free
		if p.cfg.MinFreeDisk > 0 && free < uint64(p.cfg.MinFreeDisk) {
			st.DiskOK = false
		}
	}
	return st
}
