package lease

import (
	"context"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// countExpiring counts grace-window warning events (still-active leases
// with GraceUntil stamped).
func countExpiring(events []proto.Lease) int {
	n := 0
	for _, e := range events {
		if e.State == proto.LeaseActive && e.GraceUntil != nil {
			n++
		}
	}
	return n
}

func TestRenewWithinGraceSucceeds(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	now := time.Now().UTC()
	m.now = func() time.Time { return now }
	var events []proto.Lease
	m.onEvent = func(l proto.Lease) { events = append(events, l) }

	l, err := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1", TTLSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Second) // past nominal expiry, inside grace
	m.ExpireNow()

	got, err := m.Get(l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != proto.LeaseActive || got.GraceUntil == nil {
		t.Fatalf("lease in grace = %+v", got)
	}
	if got.ExpiresInSeconds == nil || *got.ExpiresInSeconds >= 0 {
		t.Fatalf("expires_in_seconds should be negative in grace, got %+v", got.ExpiresInSeconds)
	}
	if countExpiring(events) != 1 {
		t.Fatalf("want exactly one expiring warning, events = %+v", events)
	}
	m.ExpireNow() // the warning is one-shot
	if countExpiring(events) != 1 {
		t.Fatalf("expiring warning re-fired, events = %+v", events)
	}

	renewed, err := m.Renew(l.ID, 60)
	if err != nil {
		t.Fatalf("renew within grace failed: %v", err)
	}
	if renewed.State != proto.LeaseActive || renewed.GraceUntil != nil {
		t.Fatalf("renewed = %+v", renewed)
	}
	if renewed.ExpiresInSeconds == nil || *renewed.ExpiresInSeconds <= 0 {
		t.Fatalf("renewed expires_in_seconds = %+v", renewed.ExpiresInSeconds)
	}
}

func TestGraceWindowClosesAndExpires(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	now := time.Now().UTC()
	m.now = func() time.Time { return now }

	l, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a1", TTLSeconds: 10})
	q, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "a2"})

	now = now.Add(11 * time.Second)
	m.ExpireNow() // enters grace; the queued lease must NOT be promoted yet
	if gotQ, _ := m.Get(q.ID); gotQ.State != proto.LeaseQueued {
		t.Fatalf("q promoted during grace: %+v", gotQ)
	}

	now = now.Add(DefaultRenewGrace - 10*time.Second)
	m.Get(q.ID) // the queued owner keeps polling, so it stays promotable
	now = now.Add(11 * time.Second)
	m.ExpireNow()
	if got, _ := m.Get(l.ID); got.State != proto.LeaseExpired {
		t.Fatalf("l after grace = %+v", got)
	}
	if gotQ, _ := m.Get(q.ID); gotQ.State != proto.LeaseActive {
		t.Fatalf("q after grace = %+v", gotQ)
	}
	if _, err := m.Renew(l.ID, 60); err != ErrNotActive {
		t.Fatalf("renew after grace = %v, want ErrNotActive", err)
	}
}

func TestResetDeferredDuringGrace(t *testing.T) {
	m := newTestManager(t)
	rec := newResetRecorder()
	close(rec.release)
	m.SetResetFunc(rec.fn)
	ctx := context.Background()
	now := time.Now().UTC()
	m.now = func() time.Time { return now }

	l, _ := m.Acquire(ctx, proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "a1", Reset: "erase", TTLSeconds: 10,
	})
	now = now.Add(11 * time.Second)
	m.ExpireNow()
	select {
	case got := <-rec.calls:
		t.Fatalf("reset ran during grace: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}

	now = now.Add(DefaultRenewGrace + time.Second)
	m.ExpireNow()
	got := waitForReset(t, rec)
	if got.ID != l.ID || got.Reset != "erase" {
		t.Fatalf("bad reset lease: %+v", got)
	}
}
