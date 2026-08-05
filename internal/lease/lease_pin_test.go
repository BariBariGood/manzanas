package lease

import (
	"context"
	"errors"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

func TestAcquirePinnedUDID(t *testing.T) {
	m := newTestManager(t)
	l, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{
		UDID: "UDID-2", AgentID: "a1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if l.State != proto.LeaseActive || l.TargetUDID != "UDID-2" {
		t.Fatalf("got %+v, want active on UDID-2", l)
	}
	if l.RequestedUDID != "UDID-2" {
		t.Fatalf("RequestedUDID = %q, want UDID-2", l.RequestedUDID)
	}
}

func TestAcquirePinnedUDIDWithLabels(t *testing.T) {
	m := newTestManager(t)
	// UDID-2 is iOS 18.5; pinning it with a non-matching label is no_match.
	_, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{
		UDID: "UDID-2", Labels: []string{"ios26"}, AgentID: "a1",
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v, want ErrNoMatch", err)
	}
	l, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{
		UDID: "UDID-1", Labels: []string{"ios26"}, AgentID: "a1",
	})
	if err != nil || l.TargetUDID != "UDID-1" {
		t.Fatalf("got %+v, %v; want active on UDID-1", l, err)
	}
}

func TestAcquirePinnedUnknownUDID(t *testing.T) {
	m := newTestManager(t)
	_, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{
		UDID: "UDID-NOPE", AgentID: "a1",
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v, want ErrNoMatch", err)
	}
}

func TestAcquirePinnedQueuesAndPromotes(t *testing.T) {
	m := newTestManager(t)
	first, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{
		UDID: "UDID-1", AgentID: "a1",
	})
	if err != nil || first.State != proto.LeaseActive {
		t.Fatalf("first: %+v, %v", first, err)
	}
	second, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{
		UDID: "UDID-1", AgentID: "a2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.State != proto.LeaseQueued {
		t.Fatalf("second = %+v, want queued (UDID-1 is leased)", second)
	}
	if _, err := m.Release(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	got, err := m.Get(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != proto.LeaseActive || got.TargetUDID != "UDID-1" {
		t.Fatalf("promoted = %+v, want active on UDID-1", got)
	}
}

func TestPinnedQueueDoesNotBlockOtherTargets(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{
		UDID: "UDID-1", AgentID: "a1",
	}); err != nil {
		t.Fatal(err)
	}
	// A queued pin on the busy UDID-1 must not stop an unpinned request
	// from taking the free UDID-2.
	if _, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{
		UDID: "UDID-1", AgentID: "a2",
	}); err != nil {
		t.Fatal(err)
	}
	l, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{
		AgentID: "a3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if l.State != proto.LeaseActive || l.TargetUDID != "UDID-2" {
		t.Fatalf("got %+v, want active on UDID-2", l)
	}
}
