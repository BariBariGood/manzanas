package eval

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/BariBariGood/manzanas/proto"
)

// fakeDaemon is an httptest-backed stand-in for manzanasd implementing the
// subset of the v0 REST surface the harness uses.
type fakeDaemon struct {
	mu       sync.Mutex
	server   *httptest.Server
	target   proto.Target
	leaseSeq int
	leases   map[string]*proto.Lease

	// observeTree/observeHash are returned from observe actions; tests
	// mutate them to simulate UI changes.
	observeTree map[string]any
	observeHash string
	// failActionKinds makes those action kinds return 500.
	failActionKinds map[string]bool
	// failActionsOnce makes an action kind fail N times, then succeed.
	failActionsOnce map[string]int
	// renewCount counts lease renewals.
	renewCount int
	// overloadBoots makes the next N boot requests answer 503 overloaded.
	overloadBoots int
	// bootAttempts counts boot requests (including refused ones).
	bootAttempts int
	// acquireUDIDs records the udid pin of each acquire request.
	acquireUDIDs []string
	// actionLog records dispatched action kinds.
	actionLog []string
	// fixtureLog records applied fixture names.
	fixtureLog []string
	snapshots  []proto.SnapshotInfo
	snapSeq    int
}

func newFakeDaemon() *fakeDaemon {
	fd := &fakeDaemon{
		target: proto.Target{
			UDID:    "FAKE-UDID-1",
			Kind:    proto.TargetSimulator,
			Name:    "iPhone Fake",
			Runtime: "iOS 26.5",
			State:   proto.StateShutdown,
			Labels:  []string{"simulator", "ios26"},
		},
		leases: map[string]*proto.Lease{},
		observeTree: map[string]any{
			"role": "Window",
			"children": []any{
				map[string]any{"role": "Button", "label": "General"},
			},
		},
		observeHash:     "hash-1",
		failActionKinds: map[string]bool{},
		failActionsOnce: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": "v0"})
	})
	mux.HandleFunc("GET /v0/targets", fd.handleTargets)
	mux.HandleFunc("POST /v0/targets/{udid}/boot", fd.handleBoot)
	mux.HandleFunc("POST /v0/targets/{udid}/shutdown", fd.handleShutdown)
	mux.HandleFunc("POST /v0/leases", fd.handleAcquire)
	mux.HandleFunc("GET /v0/leases/{id}", fd.handleGetLease)
	mux.HandleFunc("DELETE /v0/leases/{id}", fd.handleRelease)
	mux.HandleFunc("POST /v0/leases/{id}/renew", fd.handleRenew)
	mux.HandleFunc("DELETE /v0/state/snapshots/{id}", fd.handleDeleteSnapshot)
	mux.HandleFunc("POST /v0/actions", fd.handleAction)
	mux.HandleFunc("POST /v0/state/fixtures", fd.handleFixture)
	mux.HandleFunc("POST /v0/state/snapshots", fd.handleSnapshot)
	mux.HandleFunc("GET /v0/state/snapshots", fd.handleListSnapshots)
	mux.HandleFunc("POST /v0/state/restore", fd.handleRestore)
	mux.HandleFunc("POST /v0/state/erase", fd.handleErase)
	fd.server = httptest.NewServer(mux)
	return fd
}

func (fd *fakeDaemon) URL() string { return fd.server.URL }
func (fd *fakeDaemon) Close()      { fd.server.Close() }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, proto.Error{Code: code, Message: msg})
}

func (fd *fakeDaemon) handleTargets(w http.ResponseWriter, r *http.Request) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	writeJSON(w, 200, map[string]any{"targets": []proto.Target{fd.target}})
}

func (fd *fakeDaemon) requireLease(w http.ResponseWriter, id string) *proto.Lease {
	l, ok := fd.leases[id]
	if !ok {
		writeErr(w, 404, proto.ErrNotFound, "no such lease")
		return nil
	}
	if l.State != proto.LeaseActive {
		writeErr(w, 410, proto.ErrLeaseExpired, "lease not active")
		return nil
	}
	return l
}

func decodeBody[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		writeErr(w, 400, proto.ErrBadRequest, err.Error())
		return v, false
	}
	return v, true
}

func (fd *fakeDaemon) handleBoot(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeBody[map[string]string](w, r)
	if !ok {
		return
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	fd.bootAttempts++
	if fd.overloadBoots > 0 {
		fd.overloadBoots--
		writeErr(w, 503, proto.ErrOverloaded, "host gates refused the boot")
		return
	}
	if fd.requireLease(w, body["lease_id"]) == nil {
		return
	}
	fd.target.State = proto.StateBooted
	writeJSON(w, 202, fd.target)
}

func (fd *fakeDaemon) handleShutdown(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeBody[map[string]string](w, r)
	if !ok {
		return
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if fd.requireLease(w, body["lease_id"]) == nil {
		return
	}
	fd.target.State = proto.StateShutdown
	writeJSON(w, 202, fd.target)
}

func (fd *fakeDaemon) handleAcquire(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeBody[proto.AcquireLeaseRequest](w, r)
	if !ok {
		return
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	fd.acquireUDIDs = append(fd.acquireUDIDs, req.UDID)
	if req.UDID != "" && req.UDID != fd.target.UDID {
		writeErr(w, 409, proto.ErrNoMatch, "no target with udid "+req.UDID)
		return
	}
	for _, want := range req.Labels {
		found := false
		for _, have := range fd.target.Labels {
			if want == have {
				found = true
			}
		}
		if !found {
			writeErr(w, 409, proto.ErrNoMatch, "no target matches "+want)
			return
		}
	}
	fd.leaseSeq++
	lease := &proto.Lease{
		ID:         fmt.Sprintf("lse_%d", fd.leaseSeq),
		TargetUDID: fd.target.UDID,
		Labels:     req.Labels,
		State:      proto.LeaseActive,
		AgentID:    req.AgentID,
		Reset:      req.Reset,
		TTLSeconds: req.TTLSeconds,
	}
	fd.leases[lease.ID] = lease
	writeJSON(w, 201, lease)
}

func (fd *fakeDaemon) handleGetLease(w http.ResponseWriter, r *http.Request) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	l, ok := fd.leases[r.PathValue("id")]
	if !ok {
		writeErr(w, 404, proto.ErrNotFound, "no such lease")
		return
	}
	writeJSON(w, 200, l)
}

func (fd *fakeDaemon) handleRelease(w http.ResponseWriter, r *http.Request) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	l, ok := fd.leases[r.PathValue("id")]
	if !ok {
		writeErr(w, 404, proto.ErrNotFound, "no such lease")
		return
	}
	if l.State == proto.LeaseActive {
		l.State = proto.LeaseReleased
	}
	writeJSON(w, 200, l)
}

func (fd *fakeDaemon) handleAction(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeBody[proto.ActionRequest](w, r)
	if !ok {
		return
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if fd.requireLease(w, req.LeaseID) == nil {
		return
	}
	fd.actionLog = append(fd.actionLog, req.Kind)
	if fd.failActionKinds[req.Kind] {
		writeErr(w, 500, proto.ErrInternal, req.Kind+" failed")
		return
	}
	if n := fd.failActionsOnce[req.Kind]; n > 0 {
		fd.failActionsOnce[req.Kind] = n - 1
		writeErr(w, 500, proto.ErrInternal, req.Kind+" transiently failed")
		return
	}
	var result map[string]any
	switch req.Kind {
	case "observe":
		result = map[string]any{"tree": fd.observeTree, "hash": fd.observeHash}
	case "screenshot":
		// A 1x1 transparent PNG.
		result = map[string]any{
			"format": "png",
			"png_base64": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4" +
				"nGNgYGAAAAAEAAH2FzhVAAAAAElFTkSuQmCC",
		}
	default:
		result = map[string]any{}
	}
	writeJSON(w, 200, proto.ActionResult{OK: true, Result: result})
}

func (fd *fakeDaemon) handleFixture(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeBody[proto.FixtureRequest](w, r)
	if !ok {
		return
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if fd.requireLease(w, req.LeaseID) == nil {
		return
	}
	fd.fixtureLog = append(fd.fixtureLog, req.Name)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (fd *fakeDaemon) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeBody[proto.SnapshotRequest](w, r)
	if !ok {
		return
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if fd.requireLease(w, req.LeaseID) == nil {
		return
	}
	if fd.target.State != proto.StateShutdown {
		writeErr(w, 409, proto.ErrTargetBusy, "target is booted")
		return
	}
	fd.snapSeq++
	info := proto.SnapshotInfo{
		ID:         fmt.Sprintf("snp_%d", fd.snapSeq),
		SourceUDID: fd.target.UDID,
		Label:      req.Label,
	}
	fd.snapshots = append(fd.snapshots, info)
	writeJSON(w, 201, info)
}

func (fd *fakeDaemon) handleRenew(w http.ResponseWriter, r *http.Request) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	l, ok := fd.leases[r.PathValue("id")]
	if !ok || l.State != proto.LeaseActive {
		writeErr(w, 404, proto.ErrNotFound, "no active lease")
		return
	}
	fd.renewCount++
	writeJSON(w, 200, l)
}

func (fd *fakeDaemon) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if fd.requireLease(w, r.URL.Query().Get("lease_id")) == nil {
		return
	}
	writeJSON(w, 200, map[string]any{"snapshots": fd.snapshots})
}

func (fd *fakeDaemon) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	id := r.PathValue("id")
	for i, s := range fd.snapshots {
		if s.ID == id {
			fd.snapshots = append(fd.snapshots[:i], fd.snapshots[i+1:]...)
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	writeErr(w, 404, proto.ErrNotFound, "no such snapshot")
}

func (fd *fakeDaemon) handleRestore(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeBody[proto.RestoreRequest](w, r)
	if !ok {
		return
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if fd.requireLease(w, req.LeaseID) == nil {
		return
	}
	found := false
	for _, s := range fd.snapshots {
		if s.ID == req.Snapshot || s.Label == req.Snapshot {
			found = true
		}
	}
	if !found {
		writeErr(w, 404, proto.ErrNotFound, "no such snapshot")
		return
	}
	rebooted := false
	if fd.target.State == proto.StateBooted {
		if !req.Reboot {
			writeErr(w, 409, proto.ErrTargetBusy, "target is booted")
			return
		}
		rebooted = true
	}
	writeJSON(w, 200, proto.RestoreResult{OK: true, Snapshot: req.Snapshot, Rebooted: rebooted})
}

func (fd *fakeDaemon) handleErase(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeBody[proto.EraseRequest](w, r)
	if !ok {
		return
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if fd.requireLease(w, req.LeaseID) == nil {
		return
	}
	if fd.target.State != proto.StateShutdown {
		writeErr(w, 409, proto.ErrTargetBusy, "target is booted")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// setObserve swaps the observe response (simulating a UI change).
func (fd *fakeDaemon) setObserve(hash string, labels ...string) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	children := make([]any, len(labels))
	for i, l := range labels {
		children[i] = map[string]any{"role": "Button", "label": l}
	}
	fd.observeHash = hash
	fd.observeTree = map[string]any{"role": "Window", "children": children}
}

// snapshotCount returns the number of live snapshots.
func (fd *fakeDaemon) snapshotCount() int {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return len(fd.snapshots)
}

// releasedLeases returns IDs of leases in released state.
func (fd *fakeDaemon) releasedLeases() []string {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	var out []string
	for id, l := range fd.leases {
		if l.State == proto.LeaseReleased {
			out = append(out, id)
		}
	}
	return out
}
