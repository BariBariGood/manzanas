package broker

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// Federated runs: the broker serves the same POST /v0/runs one-call run
// primitive as a daemon by placing the run on a fleet host (the same
// warm-first ranking as lease scheduling — a run IS a lease acquire plus
// choreography) and proxying it there. The run executes entirely on the
// owning daemon; the broker only routes. Sync semantics are preserved by
// submitting async to the daemon and polling until the run finishes, so
// a multi-minute run never holds a daemon HTTP call longer than a probe.

const (
	// runPollInterval paces broker→daemon polls for sync runs.
	runPollInterval = time.Second
	// runRouteRetention bounds how long a finished run's routing entry is
	// kept; the daemon's own run retention is the real bound, this only
	// stops the table growing without limit.
	runRouteRetention = 24 * time.Hour
	// defaultRunSeconds/maxRunSeconds mirror the daemon's run budget
	// bounds (internal/server/runs.go) so the broker's sync wait always
	// outlives the daemon's own budget.
	defaultRunSeconds = 600
	maxRunSeconds     = 3480
	// runWaitMargin covers acquire queueing and daemon-side slack beyond
	// the run budget before the broker gives up waiting.
	runWaitMargin = 120 * time.Second
)

// runEntry is the broker's routing record for one placed run. loaded
// marks a run whose lease is counted in the host's intra-probe load
// bump (addLease), pending settlement when the run finishes.
type runEntry struct {
	host   *host
	at     time.Time
	loaded bool
}

// rememberRun records run→host routing, pruning stale entries. When
// loaded is set the run counts one lease against the host's placement
// load (mirroring rememberLease) until settleRun is called — a run IS a
// lease acquire, so a burst of runs within one probe interval must not
// all rank the same host first.
func (b *Broker) rememberRun(id string, h *host, loaded bool) {
	now := time.Now()
	var unload []*host
	b.mu.Lock()
	if b.runs == nil {
		b.runs = map[string]*runEntry{}
	}
	for rid, e := range b.runs {
		if now.Sub(e.at) > runRouteRetention {
			if e.loaded {
				unload = append(unload, e.host)
			}
			delete(b.runs, rid)
		}
	}
	b.runs[id] = &runEntry{host: h, at: now, loaded: loaded}
	b.mu.Unlock()
	if loaded {
		h.addLease(1)
	}
	for _, uh := range unload {
		uh.addLease(-1)
	}
}

// settleRun releases the placement-load bump for a run once it is known
// to be finished (or forgotten by its daemon). Idempotent.
func (b *Broker) settleRun(id string) {
	var h *host
	b.mu.Lock()
	if e, ok := b.runs[id]; ok && e.loaded {
		e.loaded = false
		h = e.host
	}
	b.mu.Unlock()
	if h != nil {
		h.addLease(-1)
	}
}

// loadedRuns snapshots the run IDs routed to h that still count against
// its placement load.
func (b *Broker) loadedRuns(h *host) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var ids []string
	for id, e := range b.runs {
		if e.host == h && e.loaded {
			ids = append(ids, id)
		}
	}
	return ids
}

// reconcileRuns settles loaded runs that finished without the broker
// observing it — an async run the caller never polled, a sync wait that
// hit its deadline or lost its client — so a host's placement load
// tracks reality instead of leaking until the routing entry is pruned.
func (b *Broker) reconcileRuns(ctx context.Context, h *host) {
	for _, id := range b.loadedRuns(h) {
		var run proto.Run
		err := b.withTimeout(ctx, func(ctx context.Context) error {
			return b.client.getJSON(ctx, h.cfg.Token,
				h.cfg.Addr+"/v0/runs/"+url.PathEscape(id), &run)
		})
		if err != nil {
			// Only a 404 proves the daemon forgot the run (restart, GC);
			// other wire and transport errors keep the entry so the next
			// round retries.
			var de *daemonError
			if errors.As(err, &de) && de.Status == http.StatusNotFound {
				b.settleRun(id)
			}
			continue
		}
		if run.State == proto.RunPassed || run.State == proto.RunFailed {
			b.settleRun(id)
		}
	}
}

// hostForRun returns the host a run was placed on.
func (b *Broker) hostForRun(id string) (*host, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.runs[id]
	if !ok {
		return nil, false
	}
	return e.host, true
}

// annotateRun stamps the owning host onto a run, like Lease annotation.
func (b *Broker) annotateRun(run *proto.Run, h *host) {
	run.Host = h.cfg.Name
	run.HostAddr = h.cfg.Addr
}

// runPlacementLabels derives the label set used to pick candidate hosts:
// the spec's explicit labels plus the slugs derived from runtime /
// device_type, exactly as the daemon's lease acquire will match them.
// Host-level labels may be included to pin the run to one Mac.
func runPlacementLabels(t proto.RunTarget) []string {
	labels := append([]string(nil), t.Labels...)
	for _, l := range registry.RequirementLabels(t.Runtime, t.DeviceType) {
		if !slices.Contains(labels, l) {
			labels = append(labels, l)
		}
	}
	return labels
}

// hostHasUDID reports whether the host's cached target list contains the
// pinned UDID.
func hostHasUDID(h *host, udid string) bool {
	for _, t := range h.cachedTargets() {
		if t.UDID == udid {
			return true
		}
	}
	return false
}

// handleRunCreate serves POST /v0/runs: place the run on a candidate host
// (warm-first, like lease scheduling) and proxy it there. The submission
// to the daemon is always async; a sync client request is served by
// polling the daemon until the run reaches a terminal state.
func (b *Broker) handleRunCreate(w http.ResponseWriter, r *http.Request) {
	var req proto.RunRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.NormalizeAgentID()
	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest,
			"agent_id is required (session_id is accepted as an alias)")
		return
	}
	if t := req.Spec.Target; len(t.Labels) == 0 && t.UDID == "" &&
		t.Runtime == "" && t.DeviceType == "" {
		// Mirror the daemon's validateRunSpec so a selector-less spec gets
		// the accurate 400 instead of the host-pin message below.
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest,
			"target requires at least one of labels, udid, runtime, or device_type")
		return
	}
	labels := runPlacementLabels(req.Spec.Target)
	ranked := b.rankCandidates(labels)
	candidates := make([]*host, 0, len(ranked))
	for _, rc := range ranked {
		if req.Spec.Target.UDID != "" && !hostHasUDID(rc.h, req.Spec.Target.UDID) {
			continue
		}
		candidates = append(candidates, rc.h)
	}
	if len(candidates) == 0 {
		if !b.anyHostUp() || b.downHostMatches(labels) || b.anyHostUnlisted() {
			writeError(w, http.StatusServiceUnavailable, proto.ErrUnavailable,
				"no fleet host that could match is currently reachable")
			return
		}
		writeError(w, http.StatusConflict, proto.ErrNoMatch,
			"no healthy host has a target matching the run's target requirements")
		return
	}

	var (
		daemonErr     *daemonError
		transportErr  error
		hostPinnedOne string
	)
	// Detached from the client's connection like lease acquires: once a
	// daemon accepts the run it executes (and releases its lease) whether
	// or not the client is still listening.
	submitCtx := context.WithoutCancel(r.Context())
	for _, h := range candidates {
		hreq := req
		hreq.Async = true
		spec := req.Spec
		spec.Target.Labels = stripHostLabels(labels, h)
		// Host labels select the Mac but say nothing about which of its
		// targets to lease; the daemon requires at least one target
		// selector. stripHostLabels is host-specific, so an empty result
		// only rules out this candidate — another host may keep a label
		// as a target selector.
		if len(spec.Target.Labels) == 0 && spec.Target.UDID == "" &&
			spec.Target.Runtime == "" && spec.Target.DeviceType == "" {
			hostPinnedOne = h.cfg.Name
			continue
		}
		hreq.Spec = spec
		var run proto.Run
		err := b.client.postJSON(submitCtx, h.cfg.Token, h.cfg.Addr+"/v0/runs", hreq, &run)
		if err != nil {
			var de *daemonError
			if errors.As(err, &de) {
				// Spec problems (400/501) are host-independent: forward
				// the daemon's verdict immediately. Capacity answers
				// (overloaded) and no_match try the next host.
				if de.Status == http.StatusBadRequest || de.Status == http.StatusNotImplemented {
					writeJSON(w, de.Status, de.Err)
					return
				}
				if daemonErr == nil || (de.Status >= http.StatusInternalServerError &&
					daemonErr.Status < http.StatusInternalServerError) {
					daemonErr = de
				}
				continue
			}
			b.log.Warn("run submit failed", "host", h.cfg.Name, "err", err)
			transportErr = err
			continue
		}
		b.rememberRun(run.ID, h, true)
		b.annotateRun(&run, h)
		if req.Async {
			writeJSON(w, http.StatusAccepted, run)
			return
		}
		b.waitRunSync(w, r, h, run)
		return
	}
	if transportErr != nil {
		writeError(w, http.StatusBadGateway, proto.ErrUnavailable,
			"candidate host unreachable: "+transportErr.Error())
		return
	}
	if daemonErr != nil {
		writeJSON(w, daemonErr.Status, daemonErr.Err)
		return
	}
	if hostPinnedOne != "" {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest,
			"target pinned to host "+hostPinnedOne+" only; add a target selector (labels, udid, runtime, or device_type) to choose a target on it")
		return
	}
	writeError(w, http.StatusConflict, proto.ErrNoMatch, "no host accepted the run")
}

// waitRunSync polls the owning daemon until the run finishes (or the
// client goes away / the run's budget plus margin expires), then writes
// the finished run. The daemon bounds the run by its own budget, so the
// deadline here only guards against a daemon that stops answering.
func (b *Broker) waitRunSync(w http.ResponseWriter, r *http.Request, h *host, run proto.Run) {
	budget := run.Spec.Timeouts.RunSeconds
	if budget <= 0 {
		budget = defaultRunSeconds
	}
	if budget > maxRunSeconds {
		budget = maxRunSeconds
	}
	acquire := run.Spec.Timeouts.AcquireSeconds
	deadline := time.Now().Add(time.Duration(budget+acquire)*time.Second + runWaitMargin)
	ticker := time.NewTicker(runPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return // client gone; the daemon finishes the run regardless
		case <-ticker.C:
		}
		var cur proto.Run
		err := b.client.getJSON(r.Context(), h.cfg.Token,
			h.cfg.Addr+"/v0/runs/"+url.PathEscape(run.ID), &cur)
		if err == nil {
			if cur.State == proto.RunPassed || cur.State == proto.RunFailed {
				b.settleRun(run.ID)
				b.annotateRun(&cur, h)
				writeJSON(w, http.StatusOK, cur)
				return
			}
		} else {
			var de *daemonError
			if errors.As(err, &de) && de.Status == http.StatusNotFound {
				// The daemon forgot the run (restart, GC): report it.
				b.settleRun(run.ID)
				b.annotateRun(&run, h)
				writeJSON(w, de.Status, de.Err)
				return
			}
			// Transient failure (transport or a non-404 wire error): keep
			// polling until the deadline.
		}
		if time.Now().After(deadline) {
			writeError(w, http.StatusBadGateway, proto.ErrUnavailable,
				"host "+h.cfg.Name+" stopped answering while the run was executing; poll GET /v0/runs/"+run.ID)
			return
		}
	}
}

// handleRunList serves GET /v0/runs: the union of all healthy daemons'
// retained runs, newest first, each annotated with its owning host.
func (b *Broker) handleRunList(w http.ResponseWriter, r *http.Request) {
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		merged []proto.Run
	)
	for _, h := range b.hosts {
		if !h.isUp() {
			continue
		}
		wg.Add(1)
		go func(h *host) {
			defer wg.Done()
			fctx, cancel := context.WithTimeout(r.Context(), b.probeTimeout)
			defer cancel()
			var out proto.RunList
			if err := b.client.getJSON(fctx, h.cfg.Token, h.cfg.Addr+"/v0/runs", &out); err != nil {
				var de *daemonError
				if !errors.As(err, &de) || de.Status != http.StatusNotFound {
					b.log.Warn("runs fetch failed, skipping host", "host", h.cfg.Name, "err", err)
				}
				return // older daemons without /v0/runs answer 404: skip
			}
			for i := range out.Runs {
				b.annotateRun(&out.Runs[i], h)
			}
			mu.Lock()
			merged = append(merged, out.Runs...)
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	slices.SortStableFunc(merged, func(a, c proto.Run) int {
		return c.CreatedAt.Compare(a.CreatedAt) // newest first
	})
	if merged == nil {
		merged = []proto.Run{}
	}
	writeJSON(w, http.StatusOK, proto.RunList{Runs: merged})
}

// handleRunGet serves GET /v0/runs/{id}: proxied to the owning daemon
// when the broker remembers the placement, falling back to asking each
// up host (a broker restart loses the routing table; the daemons retain
// the runs).
func (b *Broker) handleRunGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	try := func(h *host) (proto.Run, *daemonError, error) {
		var run proto.Run
		err := b.client.getJSON(r.Context(), h.cfg.Token,
			h.cfg.Addr+"/v0/runs/"+url.PathEscape(id), &run)
		if err != nil {
			var de *daemonError
			if errors.As(err, &de) {
				return run, de, nil
			}
			return run, nil, err
		}
		return run, nil, nil
	}
	if h, ok := b.hostForRun(id); ok {
		run, de, err := try(h)
		if err != nil {
			writeError(w, http.StatusBadGateway, proto.ErrUnavailable,
				"host "+h.cfg.Name+" unreachable: "+err.Error())
			return
		}
		if de != nil {
			if de.Status == http.StatusNotFound {
				b.settleRun(id)
			}
			writeJSON(w, de.Status, de.Err)
			return
		}
		if run.State == proto.RunPassed || run.State == proto.RunFailed {
			b.settleRun(id)
		}
		b.annotateRun(&run, h)
		writeJSON(w, http.StatusOK, run)
		return
	}
	// Remember the first non-404 failure: a 401/5xx from a host must not
	// be misreported as the run not existing (mirrors fanOutDo).
	var firstDE *daemonError
	var firstErr error
	for _, h := range b.hosts {
		if !h.isUp() {
			continue
		}
		run, de, err := try(h)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if de != nil {
			if de.Status != http.StatusNotFound && firstDE == nil {
				firstDE = de
			}
			continue
		}
		b.rememberRun(id, h, false)
		b.annotateRun(&run, h)
		writeJSON(w, http.StatusOK, run)
		return
	}
	if firstDE != nil {
		writeJSON(w, firstDE.Status, firstDE.Err)
		return
	}
	if firstErr != nil {
		writeError(w, http.StatusBadGateway, proto.ErrUnavailable,
			"host unreachable: "+firstErr.Error())
		return
	}
	writeError(w, http.StatusNotFound, proto.ErrNotFound, "run not known to any reachable host")
}
