package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/internal/warm"
	"github.com/BariBariGood/manzanas/proto"
)

func getStatus(t *testing.T, h http.Handler) proto.HostStatus {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var st proto.HostStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestStatusNoPool(t *testing.T) {
	reg := registry.NewMock(proto.Target{
		UDID: "AAAA-1", Kind: proto.TargetSimulator, Name: "iPhone 17",
		Runtime: "iOS 26.5", DeviceType: "iPhone 17", State: proto.StateShutdown,
	})
	s := New(reg, nil, slog.Default())
	m := lease.New(reg, s.EventSink())
	t.Cleanup(m.Close)
	s.SetLeases(m)
	h := s.Handler()

	st := getStatus(t, h)
	if st.LeasesActive != 0 || st.LeasesQueued != 0 {
		t.Fatalf("fresh daemon: %+v", st)
	}
	if !st.Gates.LoadOK || !st.Gates.DiskOK {
		t.Fatalf("no-pool gates must report ok: %+v", st.Gates)
	}

	l, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "test",
	})
	if err != nil || l.State != proto.LeaseActive {
		t.Fatalf("acquire: %v %+v", err, l)
	}
	if _, err := m.Acquire(context.Background(), proto.AcquireLeaseRequest{
		Labels: []string{"ios26"}, AgentID: "test",
	}); err != nil {
		t.Fatal(err)
	}
	st = getStatus(t, h)
	if st.LeasesActive != 1 || st.LeasesQueued != 1 {
		t.Fatalf("want 1 active + 1 queued, got %+v", st)
	}
}

func TestStatusWithPool(t *testing.T) {
	reg := registry.NewMock()
	s := New(reg, nil, slog.Default())
	m := lease.New(reg, s.EventSink())
	t.Cleanup(m.Close)
	s.SetLeases(m)
	s.SetPoolStatus(func(context.Context) warm.PoolStatus {
		return warm.PoolStatus{
			Class:         warm.CapacityClass{MaxBootedRunning: 4, MaxParked: 4, MaxConcurrentBoots: 2},
			Running:       2,
			Parked:        3,
			BootsInFlight: 1,
			LoadAvg1:      3.2,
			CPUs:          8,
			FreeDiskBytes: 91 << 30,
			LoadOK:        true,
			DiskOK:        false,
		}
	})

	st := getStatus(t, s.Handler())
	if st.Capacity.MaxBootedRunning != 4 || st.Running != 2 || st.Parked != 3 || st.BootsInFlight != 1 {
		t.Fatalf("pool fields: %+v", st)
	}
	if st.CPUs != 8 || st.LoadAvg1 != 3.2 || st.FreeDiskBytes != 91<<30 {
		t.Fatalf("gauge fields: %+v", st)
	}
	if !st.Gates.LoadOK || st.Gates.DiskOK {
		t.Fatalf("gates: %+v", st.Gates)
	}
}

func TestListTargetsMarksParkedWarm(t *testing.T) {
	reg := registry.NewMock(
		proto.Target{UDID: "AAAA-1", Kind: proto.TargetSimulator, State: proto.StateBooted, Labels: []string{"ios26"}},
		proto.Target{UDID: "AAAA-2", Kind: proto.TargetSimulator, State: proto.StateShutdown, Labels: []string{"ios26"}},
	)
	s := New(reg, nil, slog.Default())
	m := lease.New(reg, s.EventSink())
	t.Cleanup(m.Close)
	s.SetLeases(m)
	s.SetParkedCheck(func(udid string) bool { return udid == "AAAA-1" })

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/targets", nil))
	var resp struct {
		Targets []proto.Target `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	warmByUDID := map[string]bool{}
	for _, tg := range resp.Targets {
		warmByUDID[tg.UDID] = tg.Warm
	}
	if !warmByUDID["AAAA-1"] || warmByUDID["AAAA-2"] {
		t.Fatalf("warm flags: %+v", warmByUDID)
	}
}
