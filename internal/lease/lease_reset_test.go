package lease

import (
	"context"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// resetRecorder collects reset invocations and lets the test control when
// each reset "completes" (the manager frees the target via FinishReset).
type resetRecorder struct {
	calls   chan proto.Lease
	release chan struct{}
	err     error
}

func newResetRecorder() *resetRecorder {
	return &resetRecorder{calls: make(chan proto.Lease, 8), release: make(chan struct{})}
}

func (r *resetRecorder) fn(l proto.Lease) error {
	r.calls <- l
	<-r.release
	return r.err
}

func waitForReset(t *testing.T, r *resetRecorder) proto.Lease {
	t.Helper()
	select {
	case l := <-r.calls:
		return l
	case <-time.After(2 * time.Second):
		t.Fatal("reset hook not invoked")
		return proto.Lease{}
	}
}

func TestReleaseRunsResetAndHoldsTarget(t *testing.T) {
	m := newTestManager(t)
	rec := newResetRecorder()
	m.SetResetFunc(rec.fn)
	ctx := context.Background()

	l, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a1", Reset: "erase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if l.Reset != "erase" {
		t.Fatalf("Reset not carried: %+v", l)
	}
	if _, err := m.Release(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	got := waitForReset(t, rec)
	if got.ID != l.ID || got.Reset != "erase" || got.TargetUDID != l.TargetUDID {
		t.Fatalf("bad reset lease: %+v", got)
	}

	// Target must be unavailable while the reset is in flight.
	mid, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a2"})
	if err != nil {
		t.Fatal(err)
	}
	if mid.State != proto.LeaseQueued {
		t.Fatalf("acquire during reset should queue, got %+v", mid)
	}

	// Completing the reset frees the target and promotes the queued lease.
	close(rec.release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		cur, err := m.Get(mid.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.State == proto.LeaseActive {
			if cur.TargetUDID != l.TargetUDID {
				t.Fatalf("promoted to wrong target: %+v", cur)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued lease never promoted after reset: %+v", cur)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFailedResetQuarantinesTarget(t *testing.T) {
	m := newTestManager(t)
	rec := newResetRecorder()
	rec.err = context.DeadlineExceeded // any non-nil error
	close(rec.release)
	m.SetResetFunc(rec.fn)
	ctx := context.Background()

	l, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a1", Reset: "erase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	waitForReset(t, rec)

	// The dirty target must never be handed out: acquire keeps queueing.
	time.Sleep(50 * time.Millisecond)
	mid, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a2"})
	if err != nil {
		t.Fatal(err)
	}
	if mid.State != proto.LeaseQueued {
		t.Fatalf("failed reset must quarantine the target, got %+v", mid)
	}

	// An operator ClearQuarantine releases the quarantine (retry until the
	// failed reset goroutine has fully unwound).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if cleared, err := m.ClearQuarantine(l.TargetUDID); err == nil {
			if !cleared {
				t.Fatal("ClearQuarantine should report the quarantine as cleared")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ClearQuarantine still refused after reset failed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cur, err := m.Get(mid.ID)
	if err != nil || cur.State != proto.LeaseActive {
		t.Fatalf("target should be free after FinishReset: %+v %v", cur, err)
	}
}

func TestReserveTakesOverQuarantineAndRequarantines(t *testing.T) {
	m := newTestManager(t)
	rec := newResetRecorder()
	rec.err = context.DeadlineExceeded
	close(rec.release)
	m.SetResetFunc(rec.fn)
	ctx := context.Background()

	l, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a1", Reset: "erase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	waitForReset(t, rec)

	// Pool recovery may take over the quarantined target (retry until the
	// failed reset goroutine has fully unwound).
	deadline := time.Now().Add(2 * time.Second)
	for {
		takeover, ok := m.Reserve(l.TargetUDID)
		if ok {
			if !takeover {
				t.Fatal("Reserve of a quarantined target should report takeover")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Reserve should take over a quarantined target")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A failed rebuild re-quarantines: still not grantable.
	m.Quarantine(l.TargetUDID)
	mid, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a2"})
	if err != nil {
		t.Fatal(err)
	}
	if mid.State != proto.LeaseQueued {
		t.Fatalf("re-quarantined target must not be granted, got %+v", mid)
	}

	// A later successful rebuild (Reserve takeover + Unreserve) frees it.
	if takeover, ok := m.Reserve(l.TargetUDID); !ok || !takeover {
		t.Fatal("Reserve should take over the re-quarantined target")
	}
	m.Unreserve(l.TargetUDID)
	cur, err := m.Get(mid.ID)
	if err != nil || cur.State != proto.LeaseActive {
		t.Fatalf("queued lease should be promoted after Unreserve: %+v %v", cur, err)
	}
}

func TestFinishResetNoOpOnBusyTarget(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	l, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	q, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a2"})
	if err != nil || q.State != proto.LeaseQueued {
		t.Fatalf("second acquire should queue: %+v %v", q, err)
	}
	// A stray FinishReset on a target held by an active lease must not
	// promote the queued lease onto it (double assignment).
	m.FinishReset(l.TargetUDID)
	cur, err := m.Get(q.ID)
	if err != nil || cur.State != proto.LeaseQueued {
		t.Fatalf("queued lease promoted onto a busy target: %+v %v", cur, err)
	}
}

func TestClearQuarantineRefusedWhileResetInFlight(t *testing.T) {
	m := newTestManager(t)
	rec := newResetRecorder()
	m.SetResetFunc(rec.fn)
	ctx := context.Background()

	l, err := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a1", Reset: "erase",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	waitForReset(t, rec) // reset goroutine is now blocked mid-reset

	if _, err := m.ClearQuarantine(l.TargetUDID); err != ErrResetInFlight {
		t.Fatalf("want ErrResetInFlight while reset runs, got %v", err)
	}

	close(rec.release) // let the reset finish
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := m.ClearQuarantine(l.TargetUDID); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ClearQuarantine still refused after reset completed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExpiryRunsReset(t *testing.T) {
	m := newTestManager(t)
	m.SetRenewGrace(0) // exercise hard expiry, not the grace window
	rec := newResetRecorder()
	close(rec.release) // resets complete immediately
	m.SetResetFunc(rec.fn)

	now := time.Now().UTC()
	m.now = func() time.Time { return now }
	l, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a1", Reset: "snapshot:clean", TTLSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	m.ExpireNow()
	got := waitForReset(t, rec)
	if got.ID != l.ID || got.Reset != "snapshot:clean" {
		t.Fatalf("bad reset lease: %+v", got)
	}
}

func TestReleaseWithoutResetSkipsHook(t *testing.T) {
	m := newTestManager(t)
	rec := newResetRecorder()
	m.SetResetFunc(rec.fn)
	ctx := context.Background()

	l, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Release(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-rec.calls:
		t.Fatalf("unexpected reset for lease without reset spec: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	// Target is immediately free.
	l2, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a2"})
	if err != nil || l2.State != proto.LeaseActive {
		t.Fatalf("target should be free: %+v %v", l2, err)
	}
}
