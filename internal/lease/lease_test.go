package lease

import (
	"context"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

func target(udid, runtime, dt string) proto.Target {
	return proto.Target{
		UDID: udid, Kind: proto.TargetSimulator, Name: dt,
		Runtime: runtime, DeviceType: dt, State: proto.StateShutdown,
		Labels: registry.DeriveLabels("simulator", runtime, dt),
	}
}

func newTestManager(t *testing.T, targets ...proto.Target) *Manager {
	t.Helper()
	if len(targets) == 0 {
		targets = []proto.Target{
			target("UDID-1", "iOS 26.5", "iPhone 17 Pro"),
			target("UDID-2", "iOS 18.5", "iPhone 16"),
		}
	}
	// newManager (no background expiry loop) so tests can swap the clock and
	// event callback without racing a goroutine; tests drive ExpireNow directly.
	m := newManager(registry.NewMock(targets...), nil)
	t.Cleanup(m.Close)
	return m
}

func TestAcquireByLabel(t *testing.T) {
	m := newTestManager(t)
	l, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if l.State != proto.LeaseActive || l.TargetUDID != "UDID-1" {
		t.Fatalf("got %+v", l)
	}
	if l.TTLSeconds != 300 {
		t.Fatalf("default TTL = %d, want 300", l.TTLSeconds)
	}
}

func TestAcquireNoMatch(t *testing.T) {
	m := newTestManager(t)
	_, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{
		Labels: []string{"ios99"}, AgentID: "a1",
	})
	if err != ErrNoMatch {
		t.Fatalf("err = %v, want ErrNoMatch", err)
	}
}

func TestNewTargetPromotesQueuedBeforeNewcomer(t *testing.T) {
	reg := registry.NewMock(target("UDID-1", "iOS 26.5", "iPhone 17 Pro"))
	m := newManager(reg, nil)
	t.Cleanup(m.Close)
	ctx := context.Background()
	m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1"})
	q1, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a2"})
	if q1.State != proto.LeaseQueued {
		t.Fatalf("q1 = %+v", q1)
	}

	reg.Add(target("UDID-NEW", "iOS 26.5", "iPhone 17 Pro"))
	// A later request must not jump ahead of q1: the new target goes to q1.
	late, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a3"})
	if err != nil {
		t.Fatal(err)
	}
	if late.State != proto.LeaseQueued {
		t.Fatalf("late = %+v, want queued", late)
	}
	got, _ := m.Get(q1.ID)
	if got.State != proto.LeaseActive || got.TargetUDID != "UDID-NEW" {
		t.Fatalf("q1 after new target = %+v", got)
	}
}

func TestQueueFIFOAndPromoteOnRelease(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	first, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1"})
	q1, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a2"})
	q2, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a3"})
	if q1.State != proto.LeaseQueued || q1.QueuePosition != 1 {
		t.Fatalf("q1 = %+v", q1)
	}
	if q2.QueuePosition != 2 {
		t.Fatalf("q2 = %+v", q2)
	}

	if _, err := m.Release(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(q1.ID)
	if got.State != proto.LeaseActive || got.TargetUDID != first.TargetUDID {
		t.Fatalf("q1 after release = %+v", got)
	}
	got2, _ := m.Get(q2.ID)
	if got2.State != proto.LeaseQueued || got2.QueuePosition != 1 {
		t.Fatalf("q2 after release = %+v", got2)
	}
}

func TestRenewExtendsExpiry(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	l, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1", TTLSeconds: 60})
	renewed, err := m.Renew(l.ID, 600)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.TTLSeconds != 600 || !renewed.ExpiresAt.After(*l.ExpiresAt) {
		t.Fatalf("renewed = %+v", renewed)
	}
}

func TestExpiryPromotesQueued(t *testing.T) {
	m := newTestManager(t)
	m.SetRenewGrace(0) // exercise hard expiry, not the grace window
	ctx := context.Background()
	now := time.Now()
	m.now = func() time.Time { return now }

	var events []proto.Lease
	m.onEvent = func(l proto.Lease) { events = append(events, l) }

	l1, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1", TTLSeconds: 10})
	q, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a2"})

	now = now.Add(11 * time.Second)
	m.ExpireNow()

	got1, _ := m.Get(l1.ID)
	if got1.State != proto.LeaseExpired {
		t.Fatalf("l1 = %+v", got1)
	}
	gotQ, _ := m.Get(q.ID)
	if gotQ.State != proto.LeaseActive {
		t.Fatalf("q = %+v", gotQ)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
}

func TestRenewExpiredFails(t *testing.T) {
	m := newTestManager(t)
	m.SetRenewGrace(0) // exercise hard expiry, not the grace window
	ctx := context.Background()
	now := time.Now()
	m.now = func() time.Time { return now }
	l, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1", TTLSeconds: 10})
	now = now.Add(11 * time.Second)
	if _, err := m.Renew(l.ID, 60); err != ErrNotActive {
		t.Fatalf("err = %v, want ErrNotActive", err)
	}
}

func TestAbandonedQueuedLeaseExpires(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	now := time.Now()
	m.now = func() time.Time { return now }

	first, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1", TTLSeconds: 3600})
	q, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a2"})

	now = now.Add(QueueWaitTTL + time.Second)
	m.ExpireNow()

	gotQ, _ := m.Get(q.ID)
	if gotQ.State != proto.LeaseExpired {
		t.Fatalf("abandoned queued lease = %+v, want expired", gotQ)
	}
	// The freed target must not go to the expired queue entry.
	if _, err := m.Release(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, leased := m.Active(first.TargetUDID); leased {
		t.Fatal("target should be free after release with an expired queue")
	}
}

func TestReleaseQueuedLease(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1"})
	q, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a2"})
	rel, err := m.Release(ctx, q.ID)
	if err != nil || rel.State != proto.LeaseReleased {
		t.Fatalf("rel = %+v err = %v", rel, err)
	}
}

func TestTTLClamp(t *testing.T) {
	m := newTestManager(t)
	l, _ := m.Acquire(context.Background(), proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1", TTLSeconds: 999999})
	if l.TTLSeconds != 3600 {
		t.Fatalf("TTL = %d, want 3600", l.TTLSeconds)
	}
}

func TestConcurrentAcquireRelease(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				l, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"simulator"}, AgentID: "a"})
				if err != nil {
					t.Error(err)
					return
				}
				m.Release(ctx, l.ID)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if _, ok := m.Active("UDID-1"); ok {
		t.Fatal("UDID-1 still leased after all releases")
	}
}

func TestReserveBlocksAcquireAndUnreservePromotes(t *testing.T) {
	m := newTestManager(t, target("UDID-1", "iOS 26.5", "iPhone 17 Pro"))
	ctx := context.Background()

	if takeover, ok := m.Reserve("UDID-1"); !ok || takeover {
		t.Fatal("reserve of a free target failed or reported takeover")
	}
	if _, ok := m.Reserve("UDID-1"); ok {
		t.Fatal("double reserve succeeded")
	}
	q, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if q.State != proto.LeaseQueued {
		t.Fatalf("lease on reserved target = %+v, want queued", q)
	}
	if _, ok := m.Active("UDID-1"); ok {
		t.Fatal("reserved target reported an active lease")
	}

	m.Unreserve("UDID-1")
	got, err := m.Get(q.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != proto.LeaseActive || got.TargetUDID != "UDID-1" {
		t.Fatalf("after unreserve: %+v, want active on UDID-1", got)
	}
}

func TestReserveFailsOnLeasedTarget(t *testing.T) {
	m := newTestManager(t, target("UDID-1", "iOS 26.5", "iPhone 17 Pro"))
	if _, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Reserve("UDID-1"); ok {
		t.Fatal("reserve of a leased target succeeded")
	}
}
