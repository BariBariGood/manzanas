package broker

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// DefaultProbeInterval is how often each host is health-checked.
const DefaultProbeInterval = 10 * time.Second

// DefaultProbeTimeout bounds one health probe round-trip.
const DefaultProbeTimeout = 3 * time.Second

// HostHealth is the broker's view of one daemon, served at /v0/fleet/hosts.
type HostHealth struct {
	Name   string   `json:"name"`
	Addr   string   `json:"addr"`
	Labels []string `json:"labels,omitempty"`
	Up     bool     `json:"up"`
	// Build is the daemon's build version from its last successful
	// healthz probe (empty for daemons predating the field). Version
	// skew across hosts is visible by comparing this column.
	Build     string     `json:"build,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	LastProbe *time.Time `json:"last_probe,omitempty"` // nil until first probe
	// Targets is the number of targets the host reported on its last
	// successful probe.
	Targets int `json:"targets"`
	// ActiveLeases counts leases the broker has routed to this host and
	// believes are still live (active or queued).
	ActiveLeases int `json:"active_leases"`
	// Stats is the daemon's own load/occupancy snapshot from its last
	// successful GET /v0/status probe; nil for daemons that don't serve
	// the endpoint (or before the first probe). StatsAt is when it was
	// fetched.
	Stats   *proto.HostStatus `json:"stats,omitempty"`
	StatsAt *time.Time        `json:"stats_at,omitempty"`
}

// host is the broker's mutable per-daemon state.
type host struct {
	cfg HostConfig

	mu        sync.Mutex
	up        bool
	build     string // daemon build version from the last successful healthz
	lastError string
	lastProbe time.Time
	listed    bool           // a target listing has succeeded at least once
	targets   []proto.Target // last successful probe, host-annotated
	leases    int            // broker-routed live leases
	// stats is the daemon's last /v0/status snapshot; nil when the daemon
	// doesn't serve the endpoint (old daemon) or it hasn't been fetched.
	stats   *proto.HostStatus
	statsAt time.Time
	// statsBump counts leases the broker granted since stats was last
	// refreshed, so two acquires within one probe interval don't both see
	// the same stale daemon-truth load.
	statsBump int
	// adviseUnsupported marks a daemon whose POST /v0/pool/advise
	// answered 404/501 (old build); pushes are skipped until the host
	// bounces (down → up).
	adviseUnsupported bool
	// lastAdviceKey fingerprints the advice last pushed successfully;
	// advicePushed distinguishes "pushed nothing yet" from an empty push.
	lastAdviceKey string
	advicePushed  bool
	// adviceResync forces one advice push (even an empty class list)
	// after a down → up bounce: the broker can't tell a daemon restart
	// (advice copy lost) from a connectivity blip (stale copy kept), so
	// it re-syncs either way.
	adviceResync bool
	// upSince is when the host last transitioned down → up; shrink
	// advice requires the host to have been up for a full demand window.
	upSince time.Time
}

// upSinceTime returns when the host last came up, and whether it is up.
func (h *host) upSinceTime() (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.upSince, h.up
}

// adviceState returns the fingerprint of the last successful advice
// push and whether a post-bounce re-sync push is pending.
func (h *host) adviceState() (key string, pushed, resync bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastAdviceKey, h.advicePushed, h.adviceResync
}

func (h *host) setAdvicePushed(key string) {
	h.mu.Lock()
	h.lastAdviceKey = key
	h.advicePushed = true
	h.adviceResync = false
	h.mu.Unlock()
}

func (h *host) adviseUnsupportedNow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.adviseUnsupported
}

func (h *host) setAdviseUnsupported() {
	h.mu.Lock()
	h.adviseUnsupported = true
	h.mu.Unlock()
}

func (h *host) health() HostHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	return HostHealth{
		Name:      h.cfg.Name,
		Addr:      h.cfg.Addr,
		Labels:    h.cfg.Labels,
		Up:        h.up,
		Build:     h.build,
		LastError: h.lastError,
		LastProbe: func() *time.Time {
			if h.lastProbe.IsZero() {
				return nil
			}
			t := h.lastProbe
			return &t
		}(),
		// A down host offers nothing right now; its cache is kept
		// internally but not advertised as available targets.
		Targets: func() int {
			if !h.up {
				return 0
			}
			return len(h.targets)
		}(),
		ActiveLeases: func() int {
			if h.leases < 0 {
				return 0
			}
			return h.leases
		}(),
		Stats: h.stats,
		StatsAt: func() *time.Time {
			if h.stats == nil || h.statsAt.IsZero() {
				return nil
			}
			t := h.statsAt
			return &t
		}(),
	}
}

func (h *host) isUp() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.up
}

func (h *host) cachedTargets() []proto.Target {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]proto.Target, len(h.targets))
	copy(out, h.targets)
	return out
}

func (h *host) setCachedTargets(ts []proto.Target) {
	h.mu.Lock()
	h.targets = ts
	h.listed = true
	h.mu.Unlock()
}

// hasListing reports whether the host's target list has ever been
// fetched successfully; until then its (empty) cache says nothing about
// what the host offers.
func (h *host) hasListing() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listed
}

func (h *host) load() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.leases
}

func (h *host) addLease(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leases += n
	if h.leases < 0 {
		h.leases = 0
	}
	// statsBump may go negative: a release can correct a lease that is
	// still counted in the stored snapshot. effectiveLoad clamps the sum.
	h.statsBump += n
}

// statsSnapshot returns the daemon's last status snapshot, if any.
func (h *host) statsSnapshot() (proto.HostStatus, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stats == nil {
		return proto.HostStatus{}, false
	}
	return *h.stats, true
}

// setStats stores a fresh /v0/status snapshot (or clears it, for daemons
// that don't serve the endpoint) and resets the intra-interval bump.
func (h *host) setStats(st *proto.HostStatus, at time.Time) {
	h.mu.Lock()
	h.stats = st
	h.statsAt = at
	h.statsBump = 0
	h.mu.Unlock()
}

// effectiveLoad ranks the host for placement: daemon-truth
// active+queued lease counts (which see direct-to-daemon leases) plus
// the broker's own grants since the last probe, falling back to the
// broker-local counter when the daemon doesn't report status.
func (h *host) effectiveLoad() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stats == nil {
		return h.leases
	}
	if n := h.stats.LeasesActive + h.stats.LeasesQueued + h.statsBump; n > 0 {
		return n
	}
	return 0
}

// anyHostUp reports whether at least one configured host is healthy.
func (b *Broker) anyHostUp() bool {
	for _, h := range b.hosts {
		if h.isUp() {
			return true
		}
	}
	return false
}

// probe health-checks the host and refreshes its target cache.
func (b *Broker) probe(ctx context.Context, h *host) {
	outer := ctx
	ctx, cancel := context.WithTimeout(ctx, b.probeTimeout)
	defer cancel()

	var healthz struct {
		OK    bool   `json:"ok"`
		Build string `json:"build"`
	}
	err := b.client.getJSON(ctx, h.cfg.Token, h.cfg.Addr+"/v0/healthz", &healthz)
	now := time.Now().UTC()

	if err != nil || !healthz.OK {
		msg := "healthz reported not ok"
		if err != nil {
			msg = err.Error()
		}
		h.mu.Lock()
		wasUp := h.up
		h.up = false
		h.lastProbe = now
		h.lastError = msg
		h.mu.Unlock()
		if wasUp {
			b.log.Warn("host down", "host", h.cfg.Name, "err", msg)
		}
		return
	}

	// The target listing gets its own budget: it can be much slower than
	// healthz (the daemon shells out to simctl) and must not be starved by
	// whatever the health check already consumed.
	fctx, fcancel := context.WithTimeout(outer, b.probeTimeout)
	targets, err := b.fetchTargets(fctx, h)
	fcancel()
	h.mu.Lock()
	wasUp := h.up
	h.up = true
	h.build = healthz.Build
	if !wasUp {
		// A bounced host may be a new build (re-probe advise support)
		// and lost its in-memory advice copy (re-push even when the
		// hints haven't changed).
		h.adviseUnsupported = false
		h.lastAdviceKey = ""
		h.advicePushed = false
		h.adviceResync = true
		h.upSince = now
	}
	h.lastProbe = now
	if err == nil {
		h.lastError = ""
		h.targets = targets
		h.listed = true
	} else {
		// Keep the host up on the stale cache, but surface the failure.
		h.lastError = "target refresh failed: " + err.Error()
	}
	h.mu.Unlock()
	if !wasUp {
		b.log.Info("host up", "host", h.cfg.Name, "targets", len(targets))
	}

	// Reconciliation makes one request per tracked lease, so it gets the
	// loop context (with per-request timeouts) rather than sharing the
	// single-probe budget, which a busy host would exhaust mid-round.
	// It must run before the status fetch: reconciliation-driven forgets
	// refer to leases already gone from the daemon, so their addLease(-1)
	// bumps must be absorbed by the setStats reset rather than drive the
	// bump negative against a snapshot that never counted them.
	b.reconcileLeases(outer, h)
	b.reconcileRuns(outer, h)

	b.fetchStats(outer, h, now)
}

// fetchStats refreshes the host's /v0/status snapshot. Daemons that
// don't serve the endpoint (404/501 — old builds) gracefully fall back
// to no stats: placement then uses the broker-local load counter. A
// transient fetch failure keeps the previous snapshot (staleness is
// visible via stats_at).
func (b *Broker) fetchStats(ctx context.Context, h *host, now time.Time) {
	sctx, cancel := context.WithTimeout(ctx, b.probeTimeout)
	defer cancel()
	var st proto.HostStatus
	code, err := b.client.getJSONCode(sctx, h.cfg.Token, h.cfg.Addr+"/v0/status", &st)
	if err == nil {
		h.setStats(&st, now)
		return
	}
	if code == http.StatusNotFound || code == http.StatusNotImplemented {
		h.setStats(nil, now)
		return
	}
	b.log.Warn("status fetch failed; keeping previous stats", "host", h.cfg.Name, "err", err)
}

// probeLoop re-probes every host on the configured interval until ctx ends.
func (b *Broker) probeLoop(ctx context.Context) {
	ticker := time.NewTicker(b.probeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.probeAll(ctx)
		}
	}
}

// probeAll probes all hosts concurrently and waits for the round to
// finish, then pushes any changed warm-pool advice at the daemons (fresh
// stats first, so shrink advice never fires on a stale parked count).
func (b *Broker) probeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, h := range b.hosts {
		wg.Add(1)
		go func(h *host) {
			defer wg.Done()
			b.probe(ctx, h)
		}(h)
	}
	wg.Wait()
	b.adviseHosts(ctx)
}
