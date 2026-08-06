package lease

import (
	"context"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// waitForState polls Get until the lease reaches the wanted state.
func waitForState(t *testing.T, m *Manager, id string, want proto.LeaseState) proto.Lease {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		l, err := m.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if l.State == want {
			return l
		}
		if time.Now().After(deadline) {
			t.Fatalf("lease never reached %s: %+v", want, l)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A reset:"erase" acquire landing on a target dirtied by a previous lease
// must not be granted until the target has actually been erased (#85).
func TestEraseOnGrantDirtyTargetDeferred(t *testing.T) {
	m := newTestManager(t, target("UDID-1", "iOS 26.5", "iPhone 17 Pro"))
	rec := newResetRecorder()
	close(rec.release)
	m.SetResetFunc(rec.fn)
	ctx := context.Background()

	// Dirty the target: a lease with no reset leaves whatever it did.
	l1, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(ctx, l1.ID); err != nil {
		t.Fatal(err)
	}

	l2, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a2", Reset: "erase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if l2.State != proto.LeaseQueued {
		t.Fatalf("erase grant on dirty target must defer, got %+v", l2)
	}
	got := waitForReset(t, rec)
	if got.ID != l2.ID || got.Reset != "erase" || got.TargetUDID != "UDID-1" {
		t.Fatalf("bad pre-grant erase: %+v", got)
	}
	active := waitForState(t, m, l2.ID, proto.LeaseActive)
	if active.TargetUDID != "UDID-1" {
		t.Fatalf("granted wrong target: %+v", active)
	}
}

// An erase-carrying acquire prefers a clean free target over a dirty one
// so it never queues behind an avoidable pre-grant erase.
func TestEraseOnGrantPrefersCleanFreeTarget(t *testing.T) {
	m := newTestManager(t,
		target("UDID-1", "iOS 26.5", "iPhone 17 Pro"),
		target("UDID-2", "iOS 26.5", "iPhone 17 Pro"))
	rec := newResetRecorder()
	close(rec.release)
	m.SetResetFunc(rec.fn)
	ctx := context.Background()

	// Dirty UDID-1 (first free candidate) with a no-reset lease.
	l1, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a1", UDID: "UDID-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(ctx, l1.ID); err != nil {
		t.Fatal(err)
	}

	l2, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a2", Reset: "erase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if l2.State != proto.LeaseActive || l2.TargetUDID != "UDID-2" {
		t.Fatalf("erase acquire should take the clean target immediately, got %+v", l2)
	}
}

// A quarantine takeover rebuild (Reserve takeover + mandatory erase +
// Unreserve) leaves the target clean: the next reset:"erase" grant needs
// no redundant pre-grant erase.
func TestUnreserveTakeoverClearsDirty(t *testing.T) {
	m := newTestManager(t, target("UDID-1", "iOS 26.5", "iPhone 17 Pro"))
	rec := newResetRecorder()
	close(rec.release)
	rec.err = context.DeadlineExceeded // post-lease reset fails -> quarantine
	m.SetResetFunc(rec.fn)
	ctx := context.Background()

	l1, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a1", Reset: "erase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(ctx, l1.ID); err != nil {
		t.Fatal(err)
	}
	waitForReset(t, rec) // failed -> target quarantined, still dirty

	// Pool recovery: takeover of the quarantined target obliges the
	// caller to erase it before Unreserve.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if takeover, ok := m.Reserve("UDID-1"); ok {
			if !takeover {
				t.Fatal("expected takeover of quarantined target")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reserve takeover never succeeded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.Unreserve("UDID-1")

	l2, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a2", Reset: "erase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if l2.State != proto.LeaseActive {
		t.Fatalf("rebuilt target must grant without a pre-grant erase, got %+v", l2)
	}
	select {
	case extra := <-rec.calls:
		t.Fatalf("unexpected pre-grant erase after rebuild: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

// A plain (non-takeover) Reserve/Unreserve cycle does not erase the
// target, so the dirty mark must survive it: the next reset:"erase"
// grant still gets a pre-grant erase.
func TestUnreservePlainKeepsDirty(t *testing.T) {
	m := newTestManager(t, target("UDID-1", "iOS 26.5", "iPhone 17 Pro"))
	rec := newResetRecorder()
	close(rec.release)
	m.SetResetFunc(rec.fn)
	ctx := context.Background()

	l1, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(ctx, l1.ID); err != nil {
		t.Fatal(err)
	}
	// Janitor-style shutdown: reserve, no erase, unreserve.
	if takeover, ok := m.Reserve("UDID-1"); !ok || takeover {
		t.Fatalf("reserve: takeover=%v ok=%v", takeover, ok)
	}
	m.Unreserve("UDID-1")

	l2, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a2", Reset: "erase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if l2.State != proto.LeaseQueued {
		t.Fatalf("dirty target must still defer behind a pre-grant erase, got %+v", l2)
	}
	waitForReset(t, rec)
	waitForState(t, m, l2.ID, proto.LeaseActive)
}

// MarkClean (the pool's erase-then-release hook for plain holds, e.g.
// Recycle and the watchdog rebuild) clears the dirty mark so the next
// reset:"erase" grant skips the redundant pre-grant erase.
func TestMarkCleanSkipsPreGrantErase(t *testing.T) {
	m := newTestManager(t, target("UDID-1", "iOS 26.5", "iPhone 17 Pro"))
	rec := newResetRecorder()
	close(rec.release)
	m.SetResetFunc(rec.fn)
	ctx := context.Background()

	l1, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(ctx, l1.ID); err != nil {
		t.Fatal(err)
	}
	// Recycle-style rebuild: plain reserve, erase out-of-band, mark
	// clean, release.
	if takeover, ok := m.Reserve("UDID-1"); !ok || takeover {
		t.Fatalf("reserve: takeover=%v ok=%v", takeover, ok)
	}
	m.MarkClean("UDID-1")
	m.Unreserve("UDID-1")

	l2, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a2", Reset: "erase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if l2.State != proto.LeaseActive {
		t.Fatalf("marked-clean target must grant without a pre-grant erase, got %+v", l2)
	}
	select {
	case extra := <-rec.calls:
		t.Fatalf("unexpected pre-grant erase after MarkClean: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

// One queued reset:"erase" lease must trigger at most one pre-grant erase
// at a time, not fan out across every free dirty matching target.
func TestSinglePreGrantErasePerQueuedLease(t *testing.T) {
	m := newTestManager(t,
		target("UDID-1", "iOS 26.5", "iPhone 17 Pro"),
		target("UDID-2", "iOS 26.5", "iPhone 17 Pro"))
	rec := newResetRecorder() // blocks until release closed
	m.SetResetFunc(rec.fn)
	ctx := context.Background()

	// Dirty both targets.
	for _, udid := range []string{"UDID-1", "UDID-2"} {
		l, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
			Labels: []string{"ios26"}, AgentID: "a1", UDID: udid,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Release(ctx, l.ID); err != nil {
			t.Fatal(err)
		}
	}

	l2, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a2", Reset: "erase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if l2.State != proto.LeaseQueued {
		t.Fatalf("l2 = %+v", l2)
	}
	waitForReset(t, rec) // first pre-grant erase starts (blocked)

	// More acquires re-offer free targets to the queues; the queued
	// lease must not start a second erase on the other dirty target.
	if _, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"nomatch"}, AgentID: "a3",
	}); err == nil {
		t.Fatal("expected no_match")
	}
	select {
	case extra := <-rec.calls:
		t.Fatalf("second concurrent pre-grant erase started: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}

	close(rec.release)
	waitForState(t, m, l2.ID, proto.LeaseActive)
}

// A target freshly cleaned by a post-lease reset is granted to the next
// reset:"erase" lease immediately — no redundant second erase.
func TestEraseOnGrantCleanTargetSkipsErase(t *testing.T) {
	m := newTestManager(t, target("UDID-1", "iOS 26.5", "iPhone 17 Pro"))
	rec := newResetRecorder()
	close(rec.release)
	m.SetResetFunc(rec.fn)
	ctx := context.Background()

	l1, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a1", Reset: "erase",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh (never-leased) target needs no pre-grant erase.
	if l1.State != proto.LeaseActive {
		t.Fatalf("l1 = %+v", l1)
	}
	l2, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a2", Reset: "erase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if l2.State != proto.LeaseQueued {
		t.Fatalf("l2 = %+v", l2)
	}

	if _, err := m.Release(ctx, l1.ID); err != nil {
		t.Fatal(err)
	}
	// Exactly one reset runs: l1's post-lease erase. It cleans the
	// target, so l2 is promoted without a second pre-grant erase.
	got := waitForReset(t, rec)
	if got.ID != l1.ID {
		t.Fatalf("bad post-lease reset: %+v", got)
	}
	waitForState(t, m, l2.ID, proto.LeaseActive)
	select {
	case extra := <-rec.calls:
		t.Fatalf("unexpected second erase: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}
