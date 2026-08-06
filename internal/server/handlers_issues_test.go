package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/internal/warm"
	"github.com/BariBariGood/manzanas/proto"
)

// overloadRegistry refuses the first n boots with the pool's load-gate
// error, then delegates to the mock.
type overloadRegistry struct {
	*registry.MockRegistry
	mu       sync.Mutex
	refusals int
	boots    int
}

func (o *overloadRegistry) Boot(ctx context.Context, udid string) error {
	o.mu.Lock()
	o.boots++
	refuse := o.refusals > 0
	if refuse {
		o.refusals--
	}
	o.mu.Unlock()
	if refuse {
		return warm.ErrLoadTooHigh
	}
	return o.MockRegistry.Boot(ctx, udid)
}

func (o *overloadRegistry) bootCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.boots
}

func newOverloadTestServer(t *testing.T, refusals int) (*httptest.Server, *Server) {
	ts, srv, _ := newOverloadTestServerReg(t, refusals)
	return ts, srv
}

func newOverloadTestServerReg(t *testing.T, refusals int) (*httptest.Server, *Server, *overloadRegistry) {
	t.Helper()
	reg := &overloadRegistry{MockRegistry: registry.NewMock(), refusals: refusals}
	srv := New(reg, nil, nil)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv, reg
}

// Issue #86: an overload 503 must carry a Retry-After header and a
// machine-readable retry hint in the body.
func TestBootOverloadedCarriesRetryHint(t *testing.T) {
	ts, _ := newOverloadTestServer(t, 1000)

	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "test"}, &l)
	if resp.StatusCode != 201 {
		t.Fatalf("acquire: %d %+v", resp.StatusCode, l)
	}

	var e proto.Error
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/boot",
		map[string]string{"lease_id": l.ID}, &e)
	if resp.StatusCode != 503 || e.Code != proto.ErrOverloaded {
		t.Fatalf("boot: %d %+v", resp.StatusCode, e)
	}
	if e.RetryAfterSeconds <= 0 {
		t.Fatalf("missing retry_after_seconds: %+v", e)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Fatalf("missing Retry-After header")
	}
}

// Issue #86: ?wait=true retries a gate-refused boot server-side instead
// of bouncing a 503 back for the caller to busy-poll.
func TestBootWaitRidesOutOverload(t *testing.T) {
	ts, srv := newOverloadTestServer(t, 3)
	srv.bootWaitPoll = 5 * time.Millisecond

	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "test"}, &l)
	if resp.StatusCode != 201 {
		t.Fatalf("acquire: %d %+v", resp.StatusCode, l)
	}

	var booted proto.Target
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/boot?wait=true",
		map[string]string{"lease_id": l.ID}, &booted)
	if resp.StatusCode != 202 || booted.State != proto.StateBooted {
		t.Fatalf("boot with wait: %d %+v", resp.StatusCode, booted)
	}
}

// Issue #86: a boot wait whose lease ends mid-wait must abort with
// lease_expired instead of booting a target the caller no longer holds.
func TestBootWaitAbortsWhenLeaseEnds(t *testing.T) {
	ts, srv := newOverloadTestServer(t, 1000)
	srv.bootWaitPoll = 5 * time.Millisecond

	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "test"}, &l)
	if resp.StatusCode != 201 {
		t.Fatalf("acquire: %d %+v", resp.StatusCode, l)
	}

	done := make(chan struct{})
	var status int
	var e proto.Error
	go func() {
		defer close(done)
		r := doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/boot?wait=true",
			map[string]string{"lease_id": l.ID}, &e)
		status = r.StatusCode
	}()
	time.Sleep(20 * time.Millisecond)
	doJSON(t, "DELETE", ts.URL+"/v0/leases/"+l.ID, nil, nil)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("boot wait did not abort after release")
	}
	if status != 410 || e.Code != proto.ErrLeaseExpired {
		t.Fatalf("boot wait after release: %d %+v", status, e)
	}
}

// One server-side boot wait per lease: a second concurrent ?wait=true
// request on the same lease gets the hinted 503 instead of pinning
// another waiter slot and connection.
func TestBootWaitOnePerLease(t *testing.T) {
	ts, srv := newOverloadTestServer(t, 1000)
	srv.bootWaitPoll = 5 * time.Millisecond

	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "test"}, &l)
	if resp.StatusCode != 201 {
		t.Fatalf("acquire: %d %+v", resp.StatusCode, l)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		var e proto.Error
		doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/boot?wait=true",
			map[string]string{"lease_id": l.ID}, &e)
	}()
	time.Sleep(20 * time.Millisecond)

	var e proto.Error
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/boot?wait=true",
		map[string]string{"lease_id": l.ID}, &e)
	if resp.StatusCode != 503 || e.Code != proto.ErrOverloaded {
		t.Fatalf("second wait on same lease: %d %+v", resp.StatusCode, e)
	}
	if e.RetryAfterSeconds <= 0 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("second wait missing retry hint: %+v", e)
	}

	doJSON(t, "DELETE", ts.URL+"/v0/leases/"+l.ID, nil, nil)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("first wait did not abort after release")
	}
}

// A waiting boot must not run a full boot attempt (which lists every
// target on the host) on each poll while the cheap gates still refuse:
// the retry loop probes the pre-check first and only re-attempts the
// boot once it passes.
func TestBootWaitCheapGateThrottlesFullAttempts(t *testing.T) {
	ts, srv, reg := newOverloadTestServerReg(t, 1)
	srv.bootWaitPoll = time.Millisecond
	var mu sync.Mutex
	refusals := 20
	srv.SetBootGates(func(udid string) error {
		mu.Lock()
		defer mu.Unlock()
		if refusals > 0 {
			refusals--
			return warm.ErrLoadTooHigh
		}
		return nil
	})

	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "test"}, &l)
	if resp.StatusCode != 201 {
		t.Fatalf("acquire: %d %+v", resp.StatusCode, l)
	}

	var booted proto.Target
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/boot?wait=true",
		map[string]string{"lease_id": l.ID}, &booted)
	if resp.StatusCode != 202 || booted.State != proto.StateBooted {
		t.Fatalf("boot with wait: %d %+v", resp.StatusCode, booted)
	}
	if n := reg.bootCount(); n != 2 {
		t.Fatalf("full boot attempts = %d, want 2 (initial refusal + final success)", n)
	}
}

// Issue #83: a successful boot clears the shutdown ledger so a later
// not-booted error doesn't blame a shutdown that the boot undid.
func TestShutdownLedgerClearedOnBoot(t *testing.T) {
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	srv.NoteShutdown("MOCK-0000-0000-0001", "janitor", "stale reason")
	if err := srv.bootTarget(context.Background(), proto.Lease{}, "MOCK-0000-0000-0001"); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if d := srv.targetDownDetail("MOCK-0000-0000-0001"); strings.Contains(d, "janitor") {
		t.Fatalf("stale ledger entry survived boot: %q", d)
	}
}

// NoteBoot (the pool's boot-reporter hook, fired by rePark/Recycle/
// BootAsync boots that bypass the server's boot handler) must drop the
// ledger entry so a later not-booted error doesn't blame an undone
// shutdown.
func TestShutdownLedgerClearedByPoolBoot(t *testing.T) {
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	srv.NoteShutdown("MOCK-0000-0000-0001", "watchdog", "footprint recycle (erase + rebuild)")
	srv.NoteBoot("MOCK-0000-0000-0001")
	if d := srv.targetDownDetail("MOCK-0000-0000-0001"); strings.Contains(d, "watchdog") {
		t.Fatalf("stale ledger entry survived pool boot: %q", d)
	}
}

// Lease IDs are capability tokens: the shutdown ledger and every sink it
// feeds (logs, journal, the client-visible target_not_booted detail) must
// carry only the one-way digest, never any part of the raw ID.
func TestShutdownLedgerRedactsLeaseID(t *testing.T) {
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	udid := "MOCK-0000-0000-0001"
	l := proto.Lease{ID: "lse_secret1234", AgentID: "a1", TargetUDID: udid}
	if err := srv.bootTarget(context.Background(), l, udid); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := srv.shutdownTarget(context.Background(), l, udid); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	d := srv.targetDownDetail(udid)
	if strings.Contains(d, l.ID) || strings.Contains(d, "secret") {
		t.Fatalf("raw lease ID leaked into shutdown detail: %q", d)
	}
	if !strings.Contains(d, redactLeaseID(l.ID)) {
		t.Fatalf("detail missing redacted lease digest: %q", d)
	}
}

// notBootedBackend refuses every action the way a shut-down sim does.
type notBootedBackend struct{}

func (notBootedBackend) Dispatch(ctx context.Context, udid string, req proto.ActionRequest) (proto.ActionResult, error) {
	return proto.ActionResult{}, &actions.Error{Code: proto.ErrTargetNotBooted,
		Message: "target " + udid + " is not booted; boot it before dispatching actions"}
}

// Issue #83: a target_not_booted action error against a leased target
// carries a detail explaining who shut the target down and why (or that
// the daemon has no record of doing so).
func TestActionNotBootedCarriesShutdownDetail(t *testing.T) {
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	srv.SetActions(notBootedBackend{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "test"}, &l)
	if resp.StatusCode != 201 {
		t.Fatalf("acquire: %d %+v", resp.StatusCode, l)
	}

	// No ledger entry: the detail must say the daemon didn't do it.
	var e proto.Error
	resp = doJSON(t, "POST", ts.URL+"/v0/actions",
		proto.ActionRequest{LeaseID: l.ID, Kind: "tap"}, &e)
	if resp.StatusCode != 409 || e.Code != proto.ErrTargetNotBooted {
		t.Fatalf("action: %d %+v", resp.StatusCode, e)
	}
	if e.Detail == "" {
		t.Fatalf("missing detail: %+v", e)
	}

	// With a ledger entry the detail names the actor and reason.
	srv.NoteShutdown(l.TargetUDID, "janitor", "idle daemon-booted sim reclaimed")
	resp = doJSON(t, "POST", ts.URL+"/v0/actions",
		proto.ActionRequest{LeaseID: l.ID, Kind: "tap"}, &e)
	if resp.StatusCode != 409 {
		t.Fatalf("action: %d %+v", resp.StatusCode, e)
	}
	if !strings.Contains(e.Detail, "janitor") || !strings.Contains(e.Detail, "idle daemon-booted sim reclaimed") {
		t.Fatalf("detail = %q", e.Detail)
	}
}
