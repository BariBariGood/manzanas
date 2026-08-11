package broker

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/proto"
)

// queuedReconcileAfter is how long a queued lease must go without a
// client poll through the broker before reconciliation checks it. The
// daemon treats every GET /v0/leases/{id} as an owner-liveness signal
// that refreshes the queue-abandonment deadline, so probing a queued
// lease earlier would keep an abandoned request alive forever and block
// the daemon's FIFO. After QueueWaitTTL without client polls the daemon
// has already expired it, so a GET only observes the terminal state.
const queuedReconcileAfter = lease.QueueWaitTTL + 2*time.Minute

// reconcileLeases re-checks the broker's remembered leases for one host
// against the daemon and forgets the ones that are no longer live —
// leases that expired by TTL, were released by clients talking to the
// daemon directly (a documented workflow), or were GC'd by the daemon.
// Without this the lease→host table and the per-host load counters would
// grow stale, permanently skewing scheduling and /v0/fleet/hosts.
func (b *Broker) reconcileLeases(ctx context.Context, h *host) {
	b.pruneTombstones()
	b.releaseOrphans(ctx, h)
	for _, id := range b.reconcilableLeases(h) {
		var l proto.Lease
		err := b.withTimeout(ctx, func(ctx context.Context) error {
			return b.client.getJSON(ctx, h.cfg.Token, h.cfg.Addr+"/v0/leases/"+id, &l)
		})
		if err != nil {
			var de *daemonError
			if errors.As(err, &de) && de.Status == http.StatusNotFound {
				b.forgetLease(id)
			}
			// Transport errors: keep the entry; the next round retries.
			continue
		}
		if l.State == proto.LeaseReleased || l.State == proto.LeaseExpired {
			b.markLeaseTerminal(id, l.State)
			continue
		}
		b.mu.Lock()
		if e, ok := b.leases[id]; ok {
			e.state = l.State
			// The GET itself refreshed the daemon's queue-abandonment
			// deadline, so a still-queued lease must not be re-checked
			// until another full window has passed — otherwise the
			// every-round recheck becomes a self-renewing keep-alive.
			// If the client is truly gone the daemon expires the lease
			// before the next check and it is forgotten then.
			if l.State == proto.LeaseQueued {
				e.lastPoll = time.Now()
			}
		}
		b.mu.Unlock()
	}
}

// reconcilableLeases snapshots the lease IDs routed to a host that are
// safe to probe: active leases always, queued leases only once the
// client has stopped polling for longer than the daemon's abandonment
// window (see queuedReconcileAfter).
func (b *Broker) reconcilableLeases(h *host) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for id, e := range b.leases {
		if e.host != h || e.terminal() {
			continue
		}
		if e.state == proto.LeaseQueued && time.Since(e.lastPoll) < queuedReconcileAfter {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// releaseOrphans retries the release of speculative leases whose original
// release failed (see releaseQuietly), until the daemon confirms each
// lease is gone.
func (b *Broker) releaseOrphans(ctx context.Context, h *host) {
	b.mu.Lock()
	var ids []string
	for id, oh := range b.orphans {
		if oh == h {
			ids = append(ids, id)
		}
	}
	b.mu.Unlock()
	for _, id := range ids {
		err := b.withTimeout(ctx, func(ctx context.Context) error {
			return b.client.deleteJSON(ctx, h.cfg.Token, h.cfg.Addr+"/v0/leases/"+id, nil)
		})
		var de *daemonError
		if err == nil || (errors.As(err, &de) && de.Status < http.StatusInternalServerError) {
			b.mu.Lock()
			delete(b.orphans, id)
			b.mu.Unlock()
		}
	}
}

// withTimeout bounds one reconciliation request by the probe timeout, so
// each tracked lease gets its own budget instead of sharing one deadline
// across the whole round.
func (b *Broker) withTimeout(ctx context.Context, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, b.probeTimeout)
	defer cancel()
	return fn(ctx)
}

// pruneTombstones drops terminal routing entries once the owning daemon
// would have GC'd the lease anyway (lease.TerminalRetention).
func (b *Broker) pruneTombstones() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, e := range b.leases {
		if e.terminal() && time.Since(e.terminalAt) > lease.TerminalRetention {
			delete(b.leases, id)
		}
	}
}
