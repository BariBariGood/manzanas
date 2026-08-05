package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// fakeDaemon is a real manzanasd server over a mock registry in httptest —
// the broker's tests exercise the actual daemon wire protocol.
type fakeDaemon struct {
	srv    *httptest.Server
	leases *lease.Manager
}

func newFakeDaemon(t *testing.T, targets ...proto.Target) *fakeDaemon {
	t.Helper()
	reg := registry.NewMock(targets...)
	s := server.New(reg, nil, slog.Default())
	m := lease.New(reg, s.EventSink())
	t.Cleanup(m.Close)
	s.SetLeases(m)
	s.SetState(stubEngine{})
	m.SetResetFunc(s.ResetSink())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &fakeDaemon{srv: ts, leases: m}
}

// stubEngine is a no-op state engine so the fake daemons accept lease
// reset specs like the real daemon.
type stubEngine struct{}

func (stubEngine) Snapshot(context.Context, string, string) (proto.SnapshotInfo, error) {
	return proto.SnapshotInfo{}, nil
}
func (stubEngine) Restore(context.Context, string, string, bool) (bool, error) { return false, nil }
func (stubEngine) ListSnapshots(context.Context) ([]proto.SnapshotInfo, error) { return nil, nil }
func (stubEngine) DeleteSnapshot(context.Context, string) error                { return nil }
func (stubEngine) Erase(context.Context, string) error                         { return nil }
func (stubEngine) Reset(context.Context, string, string) error                 { return nil }
func (stubEngine) ApplyFixture(context.Context, string, string, map[string]any) error {
	return nil
}

// newHealthzOnlyServer answers /v0/healthz but 500s everything else,
// simulating a daemon whose target enumeration is broken.
func newHealthzOnlyServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"version":"v0"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func sim(udid, runtime, deviceType string) proto.Target {
	return proto.Target{
		UDID: udid, Kind: proto.TargetSimulator, Name: deviceType,
		Runtime: runtime, DeviceType: deviceType, State: proto.StateShutdown,
	}
}

// newTestBroker builds a broker over the given daemons and runs one probe
// round so health and target caches are warm.
func newTestBroker(t *testing.T, hosts []HostConfig) *Broker {
	t.Helper()
	b := New(Config{Hosts: hosts}, slog.Default(), Options{
		ProbeInterval: time.Hour, // tests drive probes manually
	})
	b.probeAll(context.Background())
	return b
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: bad JSON %q: %v", method, path, rec.Body.String(), err)
	}
	return rec, out
}

func acquire(t *testing.T, h http.Handler, labels ...string) (int, proto.Lease) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/leases", bytes.NewReader(mustJSON(t, proto.AcquireLeaseRequest{
		Labels: labels, AgentID: "test-agent",
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var l proto.Lease
	if rec.Code == http.StatusCreated || rec.Code == http.StatusAccepted {
		if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
			t.Fatalf("decode lease: %v", err)
		}
	}
	return rec.Code, l
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFederatedTargets(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemon(t, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"), sim("BBBB-2", "iOS 18.5", "iPhone 16"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL, Labels: []string{"intel"}},
		{Name: "two", Addr: d2.srv.URL},
	})
	h := b.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/targets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Targets []proto.Target `json:"targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Targets) != 3 {
		t.Fatalf("want 3 targets, got %d: %+v", len(resp.Targets), resp.Targets)
	}
	byUDID := map[string]proto.Target{}
	for _, tg := range resp.Targets {
		byUDID[tg.UDID] = tg
	}
	if byUDID["AAAA-1"].Host != "one" || byUDID["BBBB-1"].Host != "two" {
		t.Errorf("host annotation missing: %+v", byUDID)
	}
	if !containsAll(byUDID["AAAA-1"].Labels, []string{"intel"}) {
		t.Errorf("host labels not merged: %+v", byUDID["AAAA-1"].Labels)
	}
	// Stable order: host one's targets before host two's.
	if resp.Targets[0].UDID != "AAAA-1" {
		t.Errorf("unstable merge order: %+v", resp.Targets)
	}
}

func TestAcquireSpreadsAcrossHosts(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemon(t, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})
	h := b.Handler()

	code1, l1 := acquire(t, h, "ios26")
	code2, l2 := acquire(t, h, "ios26")
	if code1 != http.StatusCreated || code2 != http.StatusCreated {
		t.Fatalf("want two active leases, got %d/%d", code1, code2)
	}
	if l1.Host == l2.Host {
		t.Errorf("leases not spread across hosts: both on %q", l1.Host)
	}
	for _, l := range []proto.Lease{l1, l2} {
		if l.HostAddr == "" || l.Host == "" {
			t.Errorf("missing host annotation: %+v", l)
		}
		if l.State != proto.LeaseActive || l.TargetUDID == "" {
			t.Errorf("lease not active: %+v", l)
		}
	}
}

func TestAcquireByHostLabel(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemon(t, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL, Labels: []string{"intel"}},
		{Name: "two", Addr: d2.srv.URL},
	})
	h := b.Handler()

	// By host name.
	code, l := acquire(t, h, "two", "ios26")
	if code != http.StatusCreated || l.Host != "two" {
		t.Fatalf("host-name routing failed: %d %+v", code, l)
	}
	// By host extra label.
	code, l = acquire(t, h, "intel", "ios26")
	if code != http.StatusCreated || l.Host != "one" {
		t.Fatalf("host-label routing failed: %d %+v", code, l)
	}
}

func TestAcquireNoMatch(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	code, _ := acquire(t, b.Handler(), "vision-pro")
	if code != http.StatusConflict {
		t.Fatalf("want 409 no_match, got %d", code)
	}
}

func TestAcquireQueuesWhenAllBusy(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	code, _ := acquire(t, h, "ios26")
	if code != http.StatusCreated {
		t.Fatalf("first acquire: %d", code)
	}
	code, l := acquire(t, h, "ios26")
	if code != http.StatusAccepted || l.State != proto.LeaseQueued {
		t.Fatalf("want queued lease, got %d %+v", code, l)
	}
	if l.QueuePosition != 1 || l.Host != "one" {
		t.Errorf("queued lease: %+v", l)
	}
}

func TestLeaseProxyByID(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemon(t, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})
	h := b.Handler()

	code, l := acquire(t, h, "ios26")
	if code != http.StatusCreated {
		t.Fatal("acquire failed")
	}

	rec, got := doJSON(t, h, http.MethodGet, "/v0/leases/"+l.ID, nil)
	if rec.Code != http.StatusOK || got["id"] != l.ID || got["host"] != l.Host {
		t.Fatalf("get: %d %+v", rec.Code, got)
	}

	rec, got = doJSON(t, h, http.MethodPost, "/v0/leases/"+l.ID+"/renew", proto.RenewLeaseRequest{TTLSeconds: 600})
	if rec.Code != http.StatusOK || got["ttl_seconds"] != float64(600) {
		t.Fatalf("renew: %d %+v", rec.Code, got)
	}

	rec, got = doJSON(t, h, http.MethodDelete, "/v0/leases/"+l.ID, nil)
	if rec.Code != http.StatusOK || got["state"] != string(proto.LeaseReleased) {
		t.Fatalf("release: %d %+v", rec.Code, got)
	}

	// Terminal leases keep routing (daemon parity: terminal reads and
	// idempotent release), but stop counting as load.
	rec, got = doJSON(t, h, http.MethodGet, "/v0/leases/"+l.ID, nil)
	if rec.Code != http.StatusOK || got["state"] != string(proto.LeaseReleased) {
		t.Fatalf("terminal lease read through broker: %d %+v", rec.Code, got)
	}
	rec, got = doJSON(t, h, http.MethodDelete, "/v0/leases/"+l.ID, nil)
	if rec.Code != http.StatusOK || got["state"] != string(proto.LeaseReleased) {
		t.Fatalf("idempotent release through broker: %d %+v", rec.Code, got)
	}
	if load := b.hosts[0].load(); load != 0 {
		t.Fatalf("terminal lease still counted as load: %d", load)
	}

	rec, _ = doJSON(t, h, http.MethodGet, "/v0/leases/lse_unknown", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown lease: %d", rec.Code)
	}
}

func TestHostDownFailover(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemon(t, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})
	h := b.Handler()

	d1.srv.Close()
	b.probeAll(context.Background())

	// Fleet endpoint shows one down, one up.
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
	up := map[string]bool{}
	for _, hh := range fleet.Hosts {
		up[hh.Name] = hh.Up
	}
	if up["one"] || !up["two"] {
		t.Fatalf("health: %+v", fleet.Hosts)
	}
	for _, hh := range fleet.Hosts {
		if hh.Name == "one" && hh.Targets != 0 {
			t.Fatalf("down host advertises %d targets", hh.Targets)
		}
	}

	// Targets exclude the down host.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/v0/targets", nil))
	var resp struct {
		Targets []proto.Target `json:"targets"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Targets) != 1 || resp.Targets[0].Host != "two" {
		t.Fatalf("targets after host down: %+v", resp.Targets)
	}

	// Leasing keeps working on the surviving host.
	code, l := acquire(t, h, "ios26")
	if code != http.StatusCreated || l.Host != "two" {
		t.Fatalf("failover acquire: %d %+v", code, l)
	}
}

func TestSchedulerPrefersLeastLoaded(t *testing.T) {
	// Host one has 2 matching targets, host two has 2: after two leases on
	// one host path, the broker's load counters must steer to the other.
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"), sim("AAAA-2", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemon(t, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"), sim("BBBB-2", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})
	h := b.Handler()

	perHost := map[string]int{}
	for i := 0; i < 4; i++ {
		code, l := acquire(t, h, "ios26")
		if code != http.StatusCreated {
			t.Fatalf("acquire %d: %d", i, code)
		}
		perHost[l.Host]++
	}
	if perHost["one"] != 2 || perHost["two"] != 2 {
		t.Fatalf("uneven spread: %+v", perHost)
	}
}

func TestBrokerHealthz(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	rec, got := doJSON(t, b.Handler(), http.MethodGet, "/v0/healthz", nil)
	if rec.Code != http.StatusOK || got["ok"] != true || got["role"] != "broker" {
		t.Fatalf("healthz: %d %+v", rec.Code, got)
	}
}

func TestAcquireBadRequestPassthrough(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	// Missing agent_id → the daemon's bad_request must surface.
	req := httptest.NewRequest(http.MethodPost, "/v0/leases",
		bytes.NewReader([]byte(fmt.Sprintf(`{"labels":["ios26"]}`))))
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListLeasesFederated(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemon(t, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})
	h := b.Handler()

	if c, _ := acquire(t, h, "one"); c != http.StatusCreated {
		t.Fatalf("acquire one: %d", c)
	}
	if c, _ := acquire(t, h, "two"); c != http.StatusCreated {
		t.Fatalf("acquire two: %d", c)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/leases", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list leases: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Leases []proto.Lease `json:"leases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Leases) != 2 {
		t.Fatalf("want 2 leases, got %+v", resp.Leases)
	}
	if resp.Leases[0].Host != "one" || resp.Leases[1].Host != "two" {
		t.Fatalf("host order/annotation: %+v", resp.Leases)
	}
	for _, l := range resp.Leases {
		if l.ID == "" {
			t.Fatalf("lease ID missing from listing: %+v", l)
		}
		if l.HostAddr == "" {
			t.Fatalf("missing host_addr: %+v", l)
		}
	}
}

func TestUnroutedPathsUseJSONErrorEnvelope(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	cases := []struct {
		method, path string
		status       int
		code         string
	}{
		{http.MethodGet, "/v0/nonexistent", http.StatusNotFound, proto.ErrNotFound},
		{http.MethodDelete, "/v0/targets", http.StatusMethodNotAllowed, proto.ErrBadRequest},
		{http.MethodPut, "/v0/leases", http.StatusMethodNotAllowed, proto.ErrBadRequest},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.status {
			t.Fatalf("%s %s: want %d, got %d", tc.method, tc.path, tc.status, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("%s %s: content type %q", tc.method, tc.path, ct)
		}
		var e proto.Error
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Fatalf("%s %s: not JSON: %q", tc.method, tc.path, rec.Body.String())
		}
		if e.Code != tc.code {
			t.Fatalf("%s %s: code %q, want %q", tc.method, tc.path, e.Code, tc.code)
		}
	}
}
