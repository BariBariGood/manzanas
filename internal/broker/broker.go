package broker

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// Broker fronts N manzanasd daemons: federated target enumeration, lease
// scheduling across hosts, and per-lease proxying to the owning daemon.
type Broker struct {
	hosts []*host
	log   *slog.Logger

	client        *daemonClient
	probeInterval time.Duration
	probeTimeout  time.Duration

	mu      sync.Mutex
	leases  map[string]*leaseEntry // lease ID -> routing entry
	orphans map[string]*host       // speculative leases whose release failed
	rr      int                    // round-robin tiebreak counter

	// decisions is the recent-placement ring behind /v0/fleet/placements
	// and the demand stats driving rebalancing hints.
	decisions    decisionLog
	demandWindow time.Duration
}

// leaseEntry is the broker's routing record for one remembered lease.
type leaseEntry struct {
	host  *host
	state proto.LeaseState
	// lastPoll is when the client last touched the lease through the
	// broker. Queued leases are only reconciled after the daemon's
	// abandonment window has passed since lastPoll, because the daemon
	// treats every GET as an owner-liveness signal.
	lastPoll time.Time
	// terminalAt is set when the lease was observed released/expired. The
	// entry then becomes a tombstone: no longer counted as load, but still
	// routing GET/DELETE to the owning daemon (which keeps terminal leases
	// readable and treats release as idempotent) until pruned.
	terminalAt time.Time
}

func (e *leaseEntry) terminal() bool { return !e.terminalAt.IsZero() }

// Options tunes broker behavior; zero values take defaults.
type Options struct {
	ProbeInterval time.Duration
	ProbeTimeout  time.Duration
	HTTPClient    *http.Client
	// DemandWindow is how far back placement decisions count toward
	// warm-pool rebalancing hints; zero takes DefaultDemandWindow.
	DemandWindow time.Duration
}

// New creates a Broker over the configured hosts. Call Run to start the
// health prober.
func New(cfg Config, log *slog.Logger, opts Options) *Broker {
	if log == nil {
		log = slog.Default()
	}
	if opts.ProbeInterval <= 0 {
		opts.ProbeInterval = DefaultProbeInterval
	}
	if opts.ProbeTimeout <= 0 {
		opts.ProbeTimeout = DefaultProbeTimeout
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.DemandWindow <= 0 {
		opts.DemandWindow = DefaultDemandWindow
	}
	b := &Broker{
		log:           log,
		client:        &daemonClient{http: opts.HTTPClient},
		probeInterval: opts.ProbeInterval,
		probeTimeout:  opts.ProbeTimeout,
		leases:        map[string]*leaseEntry{},
		orphans:       map[string]*host{},
		demandWindow:  opts.DemandWindow,
	}
	for _, hc := range cfg.Hosts {
		b.hosts = append(b.hosts, &host{cfg: hc})
	}
	return b
}

// WarmUp synchronously probes all hosts once, so the first requests see
// fresh health and target caches. Call it before serving traffic.
func (b *Broker) WarmUp(ctx context.Context) {
	b.probeAll(ctx)
}

// Run re-probes all hosts on the configured interval until ctx is
// cancelled. Call WarmUp first so the initial requests see fresh health.
func (b *Broker) Run(ctx context.Context) {
	b.probeLoop(ctx)
}

// Handler returns the broker's HTTP handler. The overlapping surface
// (/v0/healthz, /v0/targets, /v0/leases...) speaks the same wire protocol
// as a daemon; broker-specific extras live under /v0/fleet/.
func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/healthz", b.handleHealthz)
	mux.HandleFunc("GET /v0/targets", b.handleListTargets)
	mux.HandleFunc("POST /v0/leases", b.handleAcquireLease)
	mux.HandleFunc("GET /v0/leases", b.handleListLeases)
	mux.HandleFunc("GET /v0/leases/{id}", b.handleGetLease)
	mux.HandleFunc("POST /v0/leases/{id}/renew", b.handleRenewLease)
	mux.HandleFunc("DELETE /v0/leases/{id}", b.handleReleaseLease)
	mux.HandleFunc("GET /v0/fleet/hosts", b.handleFleetHosts)
	mux.HandleFunc("GET /v0/fleet/placements", b.handleFleetPlacements)
	mux.HandleFunc("GET /v0/fleet/hints", b.handleFleetHints)
	return jsonErrorEnvelope(mux)
}

func (b *Broker) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": proto.Version,
		"role":    "broker",
		"hosts":   len(b.hosts),
	})
}

// handleFleetHosts serves GET /v0/fleet/hosts: the configured hosts with
// their current health.
func (b *Broker) handleFleetHosts(w http.ResponseWriter, r *http.Request) {
	out := make([]HostHealth, 0, len(b.hosts))
	for _, h := range b.hosts {
		out = append(out, h.health())
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": out})
}

// hostForLease returns the host owning a broker-routed lease ID.
func (b *Broker) hostForLease(id string) (*host, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.leases[id]
	if !ok {
		return nil, false
	}
	return e.host, true
}

func (b *Broker) rememberLease(l proto.Lease, h *host) {
	b.mu.Lock()
	b.leases[l.ID] = &leaseEntry{host: h, state: l.State, lastPoll: time.Now()}
	b.mu.Unlock()
	h.addLease(1)
}

// noteLeaseState records a client-driven observation of a lease's state;
// terminal states turn the entry into a tombstone (see leaseEntry) so
// repeated releases and final polls keep proxying to the owning daemon.
func (b *Broker) noteLeaseState(id string, state proto.LeaseState) {
	if state == proto.LeaseReleased || state == proto.LeaseExpired {
		b.markLeaseTerminal(id, state)
		return
	}
	b.mu.Lock()
	if e, ok := b.leases[id]; ok {
		e.state = state
		e.lastPoll = time.Now()
	}
	b.mu.Unlock()
}

// markLeaseTerminal tombstones a lease: its load is released exactly once
// but the routing entry survives until pruned, matching the daemon's
// terminal-lease retention.
func (b *Broker) markLeaseTerminal(id string, state proto.LeaseState) {
	b.mu.Lock()
	e, ok := b.leases[id]
	var release bool
	if ok {
		e.state = state
		if !e.terminal() {
			e.terminalAt = time.Now()
			release = true
		}
	}
	b.mu.Unlock()
	if release {
		e.host.addLease(-1)
	}
}

// forgetLease drops a lease entirely (the daemon no longer knows it).
func (b *Broker) forgetLease(id string) {
	b.mu.Lock()
	e, ok := b.leases[id]
	delete(b.leases, id)
	release := ok && !e.terminal()
	b.mu.Unlock()
	if release {
		e.host.addLease(-1)
	}
}

// nextRR returns a monotonically increasing counter for round-robin
// tie-breaking between equally loaded hosts.
func (b *Broker) nextRR() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rr++
	return b.rr
}
