package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// handleAcquireLease serves POST /v0/leases: pick a matching free target
// across all healthy hosts, proxy the acquire to the owning daemon, and
// return the lease annotated with host + host_addr so the client can talk
// to that daemon directly afterwards.
//
// Scheduling: candidate hosts (healthy, with at least one target matching
// the labels) are tried warm-first (see candidateHosts): hosts with a
// parked-warm or Booted matching target, then hosts with cold-boot
// headroom, then the rest; within a tier by ascending effective load
// (daemon-truth when reported), with a rotating tiebreak so equally
// ranked hosts are spread fairly. The first host that grants an active
// lease wins. If every candidate queues the request, the broker keeps
// the queued lease on the candidate with the shallowest daemon-reported
// queue and releases the rest, relying on that daemon's FIFO queue.
func (b *Broker) handleAcquireLease(w http.ResponseWriter, r *http.Request) {
	var req proto.AcquireLeaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ranked := b.rankCandidates(req.Labels)
	// record logs the decision for GET /v0/fleet/placements and the
	// demand stats behind rebalancing hints.
	record := func(outcome string, winner *host, leaseID string) {
		pd := PlacementDecision{
			At:         time.Now().UTC(),
			Labels:     req.Labels,
			Class:      classKey(req.Labels),
			Outcome:    outcome,
			LeaseID:    redactLeaseID(leaseID),
			Candidates: explainCandidates(req.Labels, ranked),
		}
		if winner != nil {
			pd.Host = winner.cfg.Name
			for _, r := range ranked {
				if r.h == winner {
					pd.Tier = tierNames[r.tier]
					break
				}
			}
		}
		b.decisions.add(pd)
	}
	candidates := make([]*host, len(ranked))
	for i, r := range ranked {
		candidates[i] = r.h
	}
	if len(candidates) == 0 {
		if !b.anyHostUp() || b.downHostMatches(req.Labels) || b.anyHostUnlisted() {
			record(proto.ErrUnavailable, nil, "")
			writeError(w, http.StatusServiceUnavailable, proto.ErrUnavailable,
				"no fleet host that could match is currently reachable")
			return
		}
		record(proto.ErrNoMatch, nil, "")
		writeError(w, http.StatusConflict, proto.ErrNoMatch,
			"no healthy host has a target matching labels")
		return
	}

	var (
		queued       *proto.Lease
		queuedHost   *host
		daemonErr    *daemonError // last wire error from a daemon
		transportErr error        // last host-unreachable error
	)
	// Acquires are detached from the client's connection: if the client
	// hangs up while a daemon is granting, the broker must still read the
	// response so the lease can be released rather than leaked. The HTTP
	// client's own timeout bounds each call.
	acquireCtx := context.WithoutCancel(r.Context())
	for _, h := range candidates {
		hreq := req
		hreq.Labels = stripHostLabels(req.Labels, h)
		var l proto.Lease
		err := b.client.postJSON(acquireCtx, h.cfg.Token, h.cfg.Addr+"/v0/leases", hreq, &l)
		if err != nil {
			var de *daemonError
			if errors.As(err, &de) {
				// Retryable failures (5xx) outrank permanent-looking
				// ones like no_match, so a transient daemon error is
				// never masked by a later host's no_match.
				if daemonErr == nil || (de.Status >= http.StatusInternalServerError &&
					daemonErr.Status < http.StatusInternalServerError) {
					daemonErr = de
				}
				continue // no_match / bad_request on this host; try the next
			}
			b.log.Warn("lease acquire failed", "host", h.cfg.Name, "err", err)
			transportErr = err
			continue
		}
		b.annotate(&l, h)
		if l.State != proto.LeaseActive {
			if queued == nil {
				queued, queuedHost = &l, h
				continue
			}
			// Two hosts queued: keep the one with the shallower
			// daemon-reported queue, release the other speculatively.
			drop, dropHost := &l, h
			if h.queuedDepth() < queuedHost.queuedDepth() {
				keep := l // copy: l is reassigned below on adoption
				drop, dropHost = queued, queuedHost
				queued, queuedHost = &keep, h
			}
			adopted := b.releaseSpeculative(r.Context(), dropHost, drop.ID, wantsReset(req.Reset))
			if adopted == nil {
				continue
			}
			// The speculative queue slot was promoted before the release
			// could land: adopt it as the active grant.
			l, h = *adopted, dropHost
		}
		if queued != nil {
			if a := b.releaseSpeculative(r.Context(), queuedHost, queued.ID, wantsReset(req.Reset)); a != nil {
				// Both grants went active: keep the earlier (FIFO) one.
				b.releaseQuietly(r.Context(), h, l.ID)
				l, h = *a, queuedHost
			}
		}
		if r.Context().Err() != nil {
			// Client gone: the lease can never be delivered.
			b.releaseQuietly(r.Context(), h, l.ID)
			return
		}
		b.rememberLease(l, h)
		record(string(proto.LeaseActive), h, l.ID)
		writeJSON(w, http.StatusCreated, l)
		return
	}
	if queued != nil {
		if r.Context().Err() != nil {
			b.releaseQuietly(r.Context(), queuedHost, queued.ID)
			return
		}
		b.rememberLease(*queued, queuedHost)
		record(string(proto.LeaseQueued), queuedHost, queued.ID)
		writeJSON(w, http.StatusAccepted, queued)
		return
	}
	// A reachability failure outranks a daemon no_match: the request might
	// have succeeded on the unreachable host, so the outcome is retryable.
	if transportErr != nil {
		record(proto.ErrUnavailable, nil, "")
		writeError(w, http.StatusBadGateway, proto.ErrUnavailable,
			"candidate host unreachable: "+transportErr.Error())
		return
	}
	if daemonErr != nil {
		record(daemonErr.Err.Code, nil, "")
		writeJSON(w, daemonErr.Status, daemonErr.Err)
		return
	}
	record(proto.ErrNoMatch, nil, "")
	writeError(w, http.StatusConflict, proto.ErrNoMatch, "no host could grant a lease")
}

// redactLeaseID maps a lease ID to a short one-way digest for the
// placement log: lease IDs are v0 capability tokens, so no part of the
// raw ID may leak, but the holder of an ID can compute the same digest
// to correlate a placement with their lease.
func redactLeaseID(id string) string {
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return "sha:" + hex.EncodeToString(sum[:4])
}

// handleListLeases serves GET /v0/leases: the union of all healthy
// daemons' lease listings, each annotated with its owning host. Hosts
// that fail to answer are skipped.
func (b *Broker) handleListLeases(w http.ResponseWriter, r *http.Request) {
	type result struct {
		idx    int
		leases []proto.Lease
	}
	results := make([]result, 0, len(b.hosts))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for i, h := range b.hosts {
		if !h.isUp() {
			continue
		}
		wg.Add(1)
		go func(i int, h *host) {
			defer wg.Done()
			fctx, cancel := context.WithTimeout(r.Context(), b.probeTimeout)
			defer cancel()
			var resp struct {
				Leases []proto.Lease `json:"leases"`
			}
			if err := b.client.getJSON(fctx, h.cfg.Token, h.cfg.Addr+"/v0/leases", &resp); err != nil {
				b.log.Warn("leases fetch failed, skipping host", "host", h.cfg.Name, "err", err)
				return
			}
			for j := range resp.Leases {
				b.annotate(&resp.Leases[j], h)
			}
			mu.Lock()
			results = append(results, result{i, resp.Leases})
			mu.Unlock()
		}(i, h)
	}
	wg.Wait()

	// Stable host order regardless of response arrival.
	merged := []proto.Lease{}
	for i := range b.hosts {
		for _, res := range results {
			if res.idx == i {
				merged = append(merged, res.leases...)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"leases": merged})
}

func (b *Broker) handleGetLease(w http.ResponseWriter, r *http.Request) {
	b.proxyLeaseOp(w, r, func(ctx context.Context, h *host, id string) (proto.Lease, error) {
		var l proto.Lease
		err := b.client.getJSON(ctx, h.cfg.Token, h.cfg.Addr+"/v0/leases/"+id, &l)
		return l, err
	})
}

func (b *Broker) handleRenewLease(w http.ResponseWriter, r *http.Request) {
	var req proto.RenewLeaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	b.proxyLeaseOp(w, r, func(ctx context.Context, h *host, id string) (proto.Lease, error) {
		var l proto.Lease
		err := b.client.postJSON(ctx, h.cfg.Token, h.cfg.Addr+"/v0/leases/"+id+"/renew", req, &l)
		return l, err
	})
}

func (b *Broker) handleReleaseLease(w http.ResponseWriter, r *http.Request) {
	b.proxyLeaseOp(w, r, func(ctx context.Context, h *host, id string) (proto.Lease, error) {
		// Detach from the client's connection: a disconnect mid-release
		// must not leave the lease holding its target until TTL expiry.
		ctx = context.WithoutCancel(ctx)
		var l proto.Lease
		err := b.client.deleteJSON(ctx, h.cfg.Token, h.cfg.Addr+"/v0/leases/"+id, &l)
		return l, err
	})
}

// proxyLeaseOp routes a per-lease op to the daemon the broker remembers as
// the lease's owner, forwards the daemon's response (including its wire
// errors), annotates it, and drops terminal leases from the routing table.
func (b *Broker) proxyLeaseOp(w http.ResponseWriter, r *http.Request,
	op func(ctx context.Context, h *host, id string) (proto.Lease, error)) {
	id := r.PathValue("id")
	h, ok := b.hostForLease(id)
	if !ok {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "lease not known to this broker")
		return
	}
	l, err := op(r.Context(), h, id)
	if err != nil {
		var de *daemonError
		if errors.As(err, &de) {
			// Only 404 proves the daemon no longer knows the lease. A 410
			// lease_expired on renew also covers merely-queued leases, so
			// the truth is left to reconciliation / a client Get.
			if de.Status == http.StatusNotFound {
				b.forgetLease(id)
			}
			writeJSON(w, de.Status, de.Err)
			return
		}
		writeError(w, http.StatusBadGateway, proto.ErrUnavailable,
			"host "+h.cfg.Name+" unreachable: "+err.Error())
		return
	}
	b.noteLeaseState(l.ID, l.State)
	b.annotate(&l, h)
	writeJSON(w, http.StatusOK, l)
}

// anyHostUnlisted reports whether some healthy host has never had a
// successful target listing: its empty cache says nothing about what it
// offers, so a failed match against it is a retryable outage, not proof
// of a label mismatch.
func (b *Broker) anyHostUnlisted() bool {
	for _, h := range b.hosts {
		if h.isUp() && !h.hasListing() {
			return true
		}
	}
	return false
}

// downHostMatches reports whether the labels select a configured host
// that is currently down — by its name, its configured extra labels, or
// its last-known target cache. Such requests are a retryable outage, not
// a label mismatch.
func (b *Broker) downHostMatches(labels []string) bool {
	for _, h := range b.hosts {
		if h.isUp() {
			continue
		}
		for _, l := range labels {
			if l == h.cfg.Name || slices.Contains(h.cfg.Labels, l) {
				return true
			}
		}
		if hostMatches(h, labels) {
			return true
		}
	}
	return false
}

// hostMatches reports whether any of the host's cached targets carries all
// requested labels. Cached targets already include the host's extra labels
// and are matched with the host name as an implicit label.
func hostMatches(h *host, labels []string) bool {
	for _, t := range h.cachedTargets() {
		all := append([]string{h.cfg.Name}, t.Labels...)
		if containsAll(all, labels) {
			return true
		}
	}
	return false
}

// stripHostLabels removes host-level labels (the host's name and its
// configured extra labels) before proxying an acquire, since the daemon
// only knows target-derived labels.
func stripHostLabels(labels []string, h *host) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == h.cfg.Name || slices.Contains(h.cfg.Labels, l) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// wantsReset reports whether an acquire requests a side-effecting
// auto-reset ("erase" or "snapshot:<name>").
func wantsReset(spec string) bool {
	return spec != "" && spec != "none"
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// annotate stamps the owning host onto a lease.
func (b *Broker) annotate(l *proto.Lease, h *host) {
	l.Host = h.cfg.Name
	l.HostAddr = h.cfg.Addr
}

// releaseSpeculative releases a speculative queued lease. When the
// acquire carries a reset spec, releasing a lease the daemon just
// promoted would run the reset and erase a simulator nobody used, so the
// lease's state is re-checked first: if it went active it is returned
// (annotated) for the caller to adopt as the winning grant instead of
// being released.
func (b *Broker) releaseSpeculative(ctx context.Context, h *host, id string, reset bool) *proto.Lease {
	if reset {
		var cur proto.Lease
		err := b.client.getJSON(context.WithoutCancel(ctx), h.cfg.Token, h.cfg.Addr+"/v0/leases/"+id, &cur)
		if err == nil && cur.State == proto.LeaseActive {
			b.annotate(&cur, h)
			return &cur
		}
	}
	b.releaseQuietly(ctx, h, id)
	return nil
}

// releaseQuietly releases a lease acquired speculatively during
// scheduling. A failed release is recorded as an orphan so each probe
// round retries it until the daemon confirms the lease is gone —
// otherwise the orphan could be promoted to a real target with no
// client behind it.
func (b *Broker) releaseQuietly(ctx context.Context, h *host, id string) {
	// Detach from the client's request: a cancelled/disconnected client
	// must not leave a speculative queued lease blocking the daemon's FIFO.
	ctx = context.WithoutCancel(ctx)
	err := b.client.deleteJSON(ctx, h.cfg.Token, h.cfg.Addr+"/v0/leases/"+id, nil)
	if err == nil {
		return
	}
	var de *daemonError
	if errors.As(err, &de) && de.Status < http.StatusInternalServerError {
		return // daemon answered: the lease is already gone/terminal
	}
	b.log.Warn("speculative lease release failed, will retry", "host", h.cfg.Name, "lease", id, "err", err)
	b.mu.Lock()
	b.orphans[id] = h
	b.mu.Unlock()
}
