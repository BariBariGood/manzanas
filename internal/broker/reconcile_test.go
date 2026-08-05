package broker

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/proto"
)

// A lease ended behind the broker's back (direct release against the
// daemon, TTL expiry, daemon GC) must be dropped from the routing table
// and load counters by the next probe round.
func TestReconcileForgetsLeasesEndedBehindBrokersBack(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	code, l := acquire(t, h, "ios26")
	if code != http.StatusCreated {
		t.Fatalf("acquire: %d", code)
	}
	if got := b.hosts[0].load(); got != 1 {
		t.Fatalf("load after acquire: %d", got)
	}

	// Client releases directly against the daemon (host_addr workflow).
	if _, err := d1.leases.Release(context.Background(), l.ID); err != nil {
		t.Fatal(err)
	}

	b.probeAll(context.Background())

	if got := b.hosts[0].load(); got != 0 {
		t.Fatalf("load after reconcile: %d", got)
	}
	// The entry survives as a tombstone (terminal reads / idempotent
	// release keep proxying) until the retention window prunes it.
	if _, ok := b.hostForLease(l.ID); !ok {
		t.Fatal("terminal lease lost its routing tombstone")
	}
	b.mu.Lock()
	b.leases[l.ID].terminalAt = time.Now().Add(-lease.TerminalRetention - time.Minute)
	b.mu.Unlock()
	b.pruneTombstones()
	if _, ok := b.hostForLease(l.ID); ok {
		t.Fatal("expired tombstone not pruned")
	}
}

// A reconciliation-driven forget refers to a lease the daemon already
// dropped, so the probe round's fresh /v0/status snapshot never counted
// it: the forget's decrement must not shift the bump against that
// snapshot, or effectiveLoad under-reports until the next probe.
func TestProbeReconcileDoesNotUnderReportEffectiveLoad(t *testing.T) {
	d1 := newFakeDaemon(t,
		sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"),
		sim("AAAA-2", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	c1, l1 := acquire(t, h, "ios26")
	c2, _ := acquire(t, h, "ios26")
	if c1 != http.StatusCreated || c2 != http.StatusCreated {
		t.Fatalf("setup acquires: %d %d", c1, c2)
	}

	// One lease ends behind the broker's back before the probe round.
	if _, err := d1.leases.Release(context.Background(), l1.ID); err != nil {
		t.Fatal(err)
	}

	b.probeAll(context.Background())

	if got := b.hosts[0].effectiveLoad(); got != 1 {
		t.Fatalf("effectiveLoad after probe = %d, want daemon-truth 1", got)
	}
}

// A speculative lease whose release failed (recorded as an orphan) must
// be re-released by the next probe round, not left to become a phantom
// holder of a real target.
func TestOrphanedSpeculativeLeaseReleasedOnProbe(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})

	l, err := d1.leases.Acquire(context.Background(), proto.AcquireLeaseRequest{
		AgentID: "agent-orphan", Labels: []string{"ios26"},
	})
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.orphans[l.ID] = b.hosts[0]
	b.mu.Unlock()

	b.probeAll(context.Background())

	got, err := d1.leases.Get(l.ID)
	if err != nil || got.State != proto.LeaseReleased {
		t.Fatalf("orphan not released on daemon: %+v %v", got, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.orphans) != 0 {
		t.Fatalf("orphan not cleared: %v", b.orphans)
	}
}

// An acquire carrying a reset spec must not leave speculative queued
// leases on non-chosen hosts: if one were promoted before its release
// landed, releasing it would erase a simulator nobody ever used.
func TestAcquireWithResetLeavesNoSpeculativeQueue(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemon(t, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})
	h := b.Handler()

	// Occupy both hosts.
	c1, l1 := acquire(t, h, "ios26")
	c2, l2 := acquire(t, h, "ios26")
	if c1 != http.StatusCreated || c2 != http.StatusCreated {
		t.Fatalf("setup acquires: %d %d", c1, c2)
	}

	rec, body := doJSON(t, h, http.MethodPost, "/v0/leases", map[string]any{
		"agent_id": "reset-agent", "labels": []string{"ios26"}, "reset": "erase",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reset acquire: %d %v", rec.Code, body)
	}
	queuedHost, _ := body["host"].(string)

	// The non-chosen daemon must hold no speculative queued lease: free
	// its target and confirm nothing gets promoted onto it.
	other, otherUDID, heldID := d2, "BBBB-1", ""
	if queuedHost == "two" {
		other, otherUDID = d1, "AAAA-1"
	}
	for _, l := range []proto.Lease{l1, l2} {
		if l.TargetUDID == otherUDID {
			heldID = l.ID
		}
	}
	if _, err := other.leases.Release(context.Background(), heldID); err != nil {
		t.Fatal(err)
	}
	if _, active := other.leases.Active(otherUDID); active {
		t.Fatal("speculative queued lease with reset was left on non-chosen host")
	}
}

// A reset-carrying acquire must still scan the whole fleet for a free
// target instead of settling for the first host's queue.
func TestAcquireWithResetFindsFreeTargetOnOtherHost(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemon(t, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})
	h := b.Handler()

	// Occupy host one directly on the daemon so the broker sees it busy.
	if _, err := d1.leases.Acquire(context.Background(), proto.AcquireLeaseRequest{
		AgentID: "holder", Labels: []string{"ios26"},
	}); err != nil {
		t.Fatal(err)
	}

	rec, body := doJSON(t, h, http.MethodPost, "/v0/leases", map[string]any{
		"agent_id": "reset-agent", "labels": []string{"ios26"}, "reset": "erase",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("want active grant on free host, got %d %v", rec.Code, body)
	}
	if body["host"] != "two" {
		t.Fatalf("want host two, got %v", body["host"])
	}
}

// Reconciliation must not poll recently-touched queued leases: the
// daemon treats each GET as an owner-liveness signal, so probing them
// would keep abandoned queue entries alive forever.
func TestReconcileSkipsFreshQueuedLeases(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	if code, _ := acquire(t, h, "ios26"); code != http.StatusCreated {
		t.Fatalf("first acquire: %d", code)
	}
	code, ql := acquire(t, h, "ios26")
	if code != http.StatusAccepted {
		t.Fatalf("second acquire: %d", code)
	}

	if ids := b.reconcilableLeases(b.hosts[0]); len(ids) != 1 {
		t.Fatalf("queued lease not excluded from reconciliation: %v", ids)
	}

	// Once the client stops polling past the daemon's abandonment window,
	// the queued lease becomes reconcilable.
	b.mu.Lock()
	b.leases[ql.ID].lastPoll = time.Now().Add(-queuedReconcileAfter - time.Minute)
	b.mu.Unlock()
	if ids := b.reconcilableLeases(b.hosts[0]); len(ids) != 2 {
		t.Fatalf("stale queued lease not reconcilable: %v", ids)
	}

	// Reconciling a still-queued lease refreshes the daemon's abandonment
	// deadline, so the broker must push its own recheck out a full window
	// rather than re-polling every round (a self-renewing keep-alive).
	b.reconcileLeases(context.Background(), b.hosts[0])
	if ids := b.reconcilableLeases(b.hosts[0]); len(ids) != 1 {
		t.Fatalf("still-queued lease re-polled immediately after reconcile: %v", ids)
	}
}

// Renewing a merely-queued lease returns the daemon's 410, but the broker
// must not forget the lease — it is still live and holding its queue slot.
func TestRenewQueuedLeaseKeepsRouting(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	if code, _ := acquire(t, h, "ios26"); code != http.StatusCreated {
		t.Fatalf("first acquire: %d", code)
	}
	code, ql := acquire(t, h, "ios26")
	if code != http.StatusAccepted {
		t.Fatalf("second acquire: %d", code)
	}

	rec, _ := doJSON(t, h, http.MethodPost, "/v0/leases/"+ql.ID+"/renew", map[string]any{"ttl_seconds": 60})
	if rec.Code != http.StatusGone {
		t.Fatalf("renew queued: %d", rec.Code)
	}
	rec, body := doJSON(t, h, http.MethodGet, "/v0/leases/"+ql.ID, nil)
	if rec.Code != http.StatusOK || body["state"] != "queued" {
		t.Fatalf("queued lease lost after renew: %d %v", rec.Code, body)
	}
	if got := b.hosts[0].load(); got != 2 {
		t.Fatalf("load after queued renew: %d", got)
	}
}

// With every host down, acquire must report a retryable 503 unavailable,
// not a permanent 409 no_match.
func TestAcquireAllHostsDownReturnsUnavailable(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	d1.srv.Close()
	b.probeAll(context.Background())

	rec, body := doJSON(t, h, http.MethodPost, "/v0/leases", map[string]any{"labels": []string{"ios26"}})
	if rec.Code != http.StatusServiceUnavailable || body["code"] != "unavailable" {
		t.Fatalf("all-hosts-down acquire: %d %v", rec.Code, body)
	}
}

// A host whose healthz answers but whose target listing fails must stay
// up (stale cache) while surfacing the failure in last_error.
func TestProbeSurfacesTargetRefreshFailure(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})

	// Simulate a broken target listing with a healthz-only daemon at the
	// same host entry.
	broken := newHealthzOnlyServer(t)
	b.hosts[0].cfg.Addr = broken.URL
	b.probeAll(context.Background())

	hh := b.hosts[0].health()
	if !hh.Up {
		t.Fatal("host should stay up on healthz success")
	}
	if hh.LastError == "" {
		t.Fatal("target refresh failure not surfaced in last_error")
	}
	if hh.Targets != 1 {
		t.Fatalf("stale target cache lost: %d", hh.Targets)
	}
}
