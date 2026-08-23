package lease

import (
	"context"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

func TestPauseAndResume(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	now := time.Now().UTC()
	m.now = func() time.Time { return now }

	l, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1", TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}

	p, err := m.Pause(l.ID, "human-present")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Paused || p.PausedAt == nil || p.PausedReason != "human-present" {
		t.Fatalf("paused lease = %+v", p)
	}
	if p.State != proto.LeaseActive || p.TargetUDID != l.TargetUDID {
		t.Fatalf("pause must keep the lease active on its target, got %+v", p)
	}

	// Pausing again is idempotent: paused_at is preserved, an empty
	// reason keeps the previous one.
	firstAt := *p.PausedAt
	now = now.Add(5 * time.Second)
	p2, err := m.Pause(l.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !p2.PausedAt.Equal(firstAt) || p2.PausedReason != "human-present" {
		t.Fatalf("re-pause changed fields: %+v", p2)
	}

	r, err := m.Resume(l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Paused || r.PausedAt != nil || r.PausedReason != "" {
		t.Fatalf("resumed lease = %+v", r)
	}
	if r.State != proto.LeaseActive {
		t.Fatalf("resume must keep the lease active, got %+v", r)
	}

	// Resuming a non-paused lease is a no-op success.
	if _, err := m.Resume(l.ID); err != nil {
		t.Fatalf("resume of non-paused lease: %v", err)
	}
}

func TestPauseErrors(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()

	if _, err := m.Pause("lse_nope", "x"); err != ErrNotFound {
		t.Fatalf("pause unknown = %v, want ErrNotFound", err)
	}
	if _, err := m.Resume("lse_nope"); err != ErrNotFound {
		t.Fatalf("resume unknown = %v, want ErrNotFound", err)
	}

	l, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1"})
	q, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, UDID: l.TargetUDID, AgentID: "a2"})
	if q.State != proto.LeaseQueued {
		t.Fatalf("second lease should queue, got %+v", q)
	}
	if _, err := m.Pause(q.ID, "x"); err != ErrNotActive {
		t.Fatalf("pause queued = %v, want ErrNotActive", err)
	}
	if _, err := m.Resume(q.ID); err != ErrNotActive {
		t.Fatalf("resume queued = %v, want ErrNotActive", err)
	}

	released, _ := m.Release(ctx, l.ID)
	if released.State != proto.LeaseReleased {
		t.Fatalf("release = %+v", released)
	}
	if _, err := m.Pause(l.ID, "x"); err != ErrNotActive {
		t.Fatalf("pause released = %v, want ErrNotActive", err)
	}
}

func TestPausedLeaseStillExpiresAndRenews(t *testing.T) {
	m := newTestManager(t)
	m.SetRenewGrace(0)
	ctx := context.Background()
	now := time.Now().UTC()
	m.now = func() time.Time { return now }

	l, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1", TTLSeconds: 10})
	if _, err := m.Pause(l.ID, "human-present"); err != nil {
		t.Fatal(err)
	}

	// A renew while paused works and does not clear the pause.
	now = now.Add(5 * time.Second)
	renewed, err := m.Renew(l.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.Paused {
		t.Fatalf("renew cleared pause: %+v", renewed)
	}

	// Pause does not stop the TTL clock: without renewals the lease
	// expires, and the terminal lease no longer reads as paused.
	now = now.Add(11 * time.Second)
	m.ExpireNow()
	got, _ := m.Get(l.ID)
	if got.State != proto.LeaseExpired {
		t.Fatalf("paused lease did not expire: %+v", got)
	}
	if got.Paused || got.PausedAt != nil || got.PausedReason != "" {
		t.Fatalf("terminal lease still reads paused: %+v", got)
	}
}

func TestReleaseClearsPause(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()

	l, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1"})
	if _, err := m.Pause(l.ID, "human-present"); err != nil {
		t.Fatal(err)
	}
	released, err := m.Release(ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if released.Paused || released.PausedAt != nil || released.PausedReason != "" {
		t.Fatalf("released lease still reads paused: %+v", released)
	}

	// The freed target is grantable again.
	l2, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, UDID: l.TargetUDID, AgentID: "a2"})
	if err != nil || l2.State != proto.LeaseActive {
		t.Fatalf("re-acquire after paused release: %v %+v", err, l2)
	}
	if l2.Paused {
		t.Fatalf("fresh lease inherited pause: %+v", l2)
	}
}
