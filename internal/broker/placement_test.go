package broker

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/internal/server"
	"github.com/BariBariGood/manzanas/proto"
)

// fakeOpts tunes a fake daemon for placement tests.
type fakeOpts struct {
	// status is served verbatim at GET /v0/status, overriding the real
	// handler; nil keeps the real (lease-count) handler.
	status *proto.HostStatus
	// noStatus makes /v0/status answer a plain 404, simulating an old
	// daemon build without the endpoint.
	noStatus bool
	// parked marks targets warm in listings, like a daemon's warm pool.
	parked func(udid string) bool
	// noAdvise makes POST /v0/pool/advise answer a plain 404, simulating
	// an old daemon build without the endpoint.
	noAdvise bool
}

func newFakeDaemonOpts(t *testing.T, o fakeOpts, targets ...proto.Target) *fakeDaemon {
	t.Helper()
	reg := registry.NewMock(targets...)
	s := server.New(reg, nil, slog.Default())
	m := lease.New(reg, s.EventSink())
	t.Cleanup(m.Close)
	s.SetLeases(m)
	s.SetState(stubEngine{})
	m.SetResetFunc(s.ResetSink())
	if o.parked != nil {
		s.SetParkedCheck(o.parked)
	}
	inner := s.Handler()
	mux := http.NewServeMux()
	if o.status != nil || o.noStatus {
		mux.HandleFunc("GET /v0/status", func(w http.ResponseWriter, r *http.Request) {
			if o.noStatus {
				http.NotFound(w, r) // plain-text 404, like a pre-status daemon
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(o.status)
		})
	}
	if o.noAdvise {
		mux.HandleFunc("POST /v0/pool/advise", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r) // plain-text 404, like a pre-advise daemon
		})
	}
	mux.Handle("/", inner)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &fakeDaemon{srv: ts, leases: m}
}

func okGates() proto.HostGates { return proto.HostGates{LoadOK: true, DiskOK: true} }

func TestPlacementPrefersDaemonTruthLoad(t *testing.T) {
	// Host one reports 3 leases the broker never routed (direct-to-daemon
	// load); host two reports none. Placement must land on two, even
	// though the broker-local counters are equal.
	d1 := newFakeDaemonOpts(t, fakeOpts{status: &proto.HostStatus{
		LeasesActive: 3, Gates: okGates(),
	}}, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemonOpts(t, fakeOpts{status: &proto.HostStatus{
		Gates: okGates(),
	}}, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})

	for i := 0; i < 2; i++ {
		code, l := acquire(t, b.Handler(), "ios26")
		if code != http.StatusCreated && code != http.StatusAccepted {
			t.Fatalf("acquire %d: %d", i, code)
		}
		if i == 0 && l.Host != "two" {
			t.Fatalf("first acquire landed on %q, want the less-loaded host", l.Host)
		}
	}
}

func TestPlacementWarmFirstTiering(t *testing.T) {
	// Host one has a warm (parked) matching target but higher load; host
	// two is idle but cold. Warm tier must outrank load.
	d1 := newFakeDaemonOpts(t, fakeOpts{
		status: &proto.HostStatus{LeasesActive: 1, Parked: 1, Gates: okGates()},
		parked: func(udid string) bool { return udid == "AAAA-1" },
	}, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemonOpts(t, fakeOpts{status: &proto.HostStatus{
		Gates: okGates(),
	}}, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})

	code, l := acquire(t, b.Handler(), "ios26")
	if code != http.StatusCreated || l.Host != "one" {
		t.Fatalf("warm host not preferred: %d %+v", code, l)
	}
}

func TestPlacementRanksSaturatedHostLast(t *testing.T) {
	// Host one is at its running cap with no warm match: an acquire there
	// is guaranteed to queue, so host two must be tried first.
	d1 := newFakeDaemonOpts(t, fakeOpts{status: &proto.HostStatus{
		Capacity: proto.HostCapacity{MaxBootedRunning: 2},
		Running:  2,
		Gates:    okGates(),
	}}, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemonOpts(t, fakeOpts{status: &proto.HostStatus{
		LeasesActive: 1, Gates: okGates(),
	}}, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})

	code, l := acquire(t, b.Handler(), "ios26")
	if code != http.StatusCreated || l.Host != "two" {
		t.Fatalf("saturated host not ranked last: %d %+v", code, l)
	}
}

func TestPlacementSkipsGateFailingHostForColdBoot(t *testing.T) {
	d1 := newFakeDaemonOpts(t, fakeOpts{status: &proto.HostStatus{
		Gates: proto.HostGates{LoadOK: false, DiskOK: true},
	}}, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemonOpts(t, fakeOpts{status: &proto.HostStatus{
		LeasesActive: 2, Gates: okGates(),
	}}, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})

	code, l := acquire(t, b.Handler(), "ios26")
	if code != http.StatusCreated || l.Host != "two" {
		t.Fatalf("gate-failing host not ranked last: %d %+v", code, l)
	}
}

func TestQueuePlacementPicksShallowestQueue(t *testing.T) {
	// Both hosts' only matching target is leased, so both queue. The 202
	// must land on the host reporting the shallower daemon queue (two),
	// not the least-loaded one (one).
	d1 := newFakeDaemonOpts(t, fakeOpts{status: &proto.HostStatus{
		LeasesActive: 1, LeasesQueued: 4, Gates: okGates(),
	}}, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemonOpts(t, fakeOpts{status: &proto.HostStatus{
		LeasesActive: 5, LeasesQueued: 1, Gates: okGates(),
	}}, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})
	h := b.Handler()

	// Occupy both targets directly on the daemons so the broker's next
	// acquire queues everywhere.
	for _, d := range []*fakeDaemon{d1, d2} {
		if _, err := d.leases.Acquire(context.Background(), proto.AcquireLeaseRequest{
			Labels: []string{"ios26"}, AgentID: "occupier",
		}); err != nil {
			t.Fatal(err)
		}
	}

	code, l := acquire(t, h, "ios26")
	if code != http.StatusAccepted || l.State != proto.LeaseQueued {
		t.Fatalf("want queued lease, got %d %+v", code, l)
	}
	if l.Host != "two" {
		t.Fatalf("queued on %q, want the shallowest-queue host \"two\"", l.Host)
	}
}

func TestEffectiveLoadReleaseAfterSnapshot(t *testing.T) {
	// A lease counted in the stored snapshot ends before the next probe:
	// the decrement must offset the snapshot (bump goes negative), so the
	// host isn't over-reported as loaded for up to a probe interval.
	h := &host{}
	h.addLease(1)
	h.setStats(&proto.HostStatus{LeasesActive: 1}, time.Now())
	if got := h.effectiveLoad(); got != 1 {
		t.Fatalf("after snapshot: effectiveLoad = %d, want 1", got)
	}
	h.addLease(-1)
	if got := h.effectiveLoad(); got != 0 {
		t.Fatalf("after release: effectiveLoad = %d, want 0", got)
	}
	// A further stray decrement must still clamp at zero.
	h.addLease(-1)
	if got := h.effectiveLoad(); got != 0 {
		t.Fatalf("after extra release: effectiveLoad = %d, want 0", got)
	}
	// The next probe resets the bump.
	h.setStats(&proto.HostStatus{LeasesActive: 2}, time.Now())
	if got := h.effectiveLoad(); got != 2 {
		t.Fatalf("after next snapshot: effectiveLoad = %d, want 2", got)
	}
}

func TestOldDaemonWithoutStatusFallsBack(t *testing.T) {
	// A pre-status daemon (plain 404) must keep working: no stats in
	// /v0/fleet/hosts, placement on the broker-local counter.
	d1 := newFakeDaemonOpts(t, fakeOpts{noStatus: true},
		sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	rec, _ := doJSON(t, h, http.MethodGet, "/v0/fleet/hosts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fleet hosts: %d", rec.Code)
	}
	var fleet struct {
		Hosts []HostHealth `json:"hosts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fleet); err != nil {
		t.Fatal(err)
	}
	if len(fleet.Hosts) != 1 || fleet.Hosts[0].Stats != nil {
		t.Fatalf("old daemon must have nil stats: %+v", fleet.Hosts)
	}
	if !fleet.Hosts[0].Up {
		t.Fatalf("old daemon marked down: %+v", fleet.Hosts[0])
	}

	code, l := acquire(t, h, "ios26")
	if code != http.StatusCreated || l.State != proto.LeaseActive {
		t.Fatalf("acquire against old daemon: %d %+v", code, l)
	}
}

func TestFleetHostsIncludesStats(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	if c, _ := acquire(t, h, "ios26"); c != http.StatusCreated {
		t.Fatalf("acquire: %d", c)
	}
	b.probeAll(context.Background())

	rec, _ := doJSON(t, h, http.MethodGet, "/v0/fleet/hosts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fleet hosts: %d", rec.Code)
	}
	var fleet struct {
		Hosts []HostHealth `json:"hosts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fleet); err != nil {
		t.Fatal(err)
	}
	hh := fleet.Hosts[0]
	if hh.Stats == nil || hh.StatsAt == nil {
		t.Fatalf("stats missing: %+v", hh)
	}
	if hh.Stats.LeasesActive != 1 {
		t.Fatalf("daemon-truth active count: %+v", hh.Stats)
	}
}
