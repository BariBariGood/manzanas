package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

func TestPlacementExplainRecordsDecision(t *testing.T) {
	d1 := newFakeDaemonOpts(t, fakeOpts{
		status: &proto.HostStatus{Parked: 1, Gates: okGates()},
		parked: func(udid string) bool { return udid == "AAAA-1" },
	}, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemonOpts(t, fakeOpts{status: &proto.HostStatus{
		Gates: okGates(),
	}}, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})
	h := b.Handler()

	code, l := acquire(t, h, "ios26")
	if code != http.StatusCreated || l.Host != "one" {
		t.Fatalf("warm host not preferred: %d %+v", code, l)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/fleet/placements", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("placements: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Placements []PlacementDecision `json:"placements"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Placements) != 1 {
		t.Fatalf("want 1 decision, got %d", len(resp.Placements))
	}
	pd := resp.Placements[0]
	if pd.Outcome != "active" || pd.Host != "one" || pd.Tier != "warm" {
		t.Fatalf("decision: %+v", pd)
	}
	if pd.Class != "ios26" || len(pd.Candidates) != 2 {
		t.Fatalf("decision detail: %+v", pd)
	}
	if pd.LeaseID == "" || strings.Contains(pd.LeaseID, strings.TrimPrefix(l.ID, "lse_")) {
		t.Fatalf("lease ID must be a one-way digest, got %q (full %q)", pd.LeaseID, l.ID)
	}
	if pd.LeaseID != redactLeaseID(l.ID) {
		t.Fatalf("digest mismatch: %q != %q", pd.LeaseID, redactLeaseID(l.ID))
	}
	best := pd.Candidates[0]
	if best.Host != "one" || best.Tier != "warm" || !best.WarmMatch || best.WarmIdle != 1 {
		t.Fatalf("best candidate: %+v", best)
	}
	if !best.HasStats || best.Parked != 1 {
		t.Fatalf("candidate stats: %+v", best)
	}
	if second := pd.Candidates[1]; second.Host != "two" || second.Tier != "headroom" {
		t.Fatalf("second candidate: %+v", second)
	}
}

func TestPlacementsEndpointBounds(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"),
		sim("AAAA-2", "iOS 26.5", "iPhone 17 Pro"), sim("AAAA-3", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	for i := 0; i < 3; i++ {
		if c, _ := acquire(t, h, "ios26"); c != http.StatusCreated {
			t.Fatalf("acquire %d: %d", i, c)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/fleet/placements?n=2", nil))
	var resp struct {
		Placements []PlacementDecision `json:"placements"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Placements) != 2 {
		t.Fatalf("want 2 decisions, got %d", len(resp.Placements))
	}
	if !resp.Placements[0].At.After(resp.Placements[1].At) &&
		!resp.Placements[0].At.Equal(resp.Placements[1].At) {
		t.Fatalf("not newest first: %+v", resp.Placements)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/fleet/placements?n=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad n: %d", rec.Code)
	}
}

func TestWarmIdleSteersEqualRank(t *testing.T) {
	// Both hosts are warm-tier with equal load; the host with the deeper
	// matching parked pool (two, 2 warm sims) must attract the lease.
	d1 := newFakeDaemonOpts(t, fakeOpts{
		status: &proto.HostStatus{Parked: 1, Gates: okGates()},
		parked: func(udid string) bool { return udid == "AAAA-1" },
	}, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemonOpts(t, fakeOpts{
		status: &proto.HostStatus{Parked: 2, Gates: okGates()},
		parked: func(udid string) bool { return true },
	}, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"), sim("BBBB-2", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})

	// Repeated ranking must always put "two" first: warm-idle depth is a
	// deterministic key, not a rotating tie.
	for i := 0; i < 3; i++ {
		ranked := b.rankCandidates([]string{"ios26"})
		if len(ranked) != 2 || ranked[0].h.cfg.Name != "two" {
			t.Fatalf("round %d: deeper warm pool not preferred: %+v", i, ranked)
		}
	}
}

func TestHintsGrowAfterColdPlacements(t *testing.T) {
	// One matching target, no warm pool: every acquire is a cold-tier
	// placement. After growColdThreshold of them the broker must advise
	// the host to grow the class.
	d1 := newFakeDaemon(t,
		sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"), sim("AAAA-2", "iOS 26.5", "iPhone 17 Pro"),
		sim("AAAA-3", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	for i := 0; i < growColdThreshold; i++ {
		if c, _ := acquire(t, h, "ios26"); c != http.StatusCreated {
			t.Fatalf("acquire %d: %d", i, c)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/fleet/hints", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("hints: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		WindowSeconds int         `json:"window_seconds"`
		Hosts         []HostHints `json:"hosts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.WindowSeconds != int(DefaultDemandWindow/time.Second) {
		t.Fatalf("window: %d", resp.WindowSeconds)
	}
	if len(resp.Hosts) != 1 || resp.Hosts[0].Host != "one" {
		t.Fatalf("hints hosts: %+v", resp.Hosts)
	}
	var grow *proto.PoolClassAdvice
	for i, c := range resp.Hosts[0].Classes {
		if c.Action == proto.AdviceGrow {
			grow = &resp.Hosts[0].Classes[i]
		}
	}
	if grow == nil || grow.ColdPlacements != growColdThreshold ||
		len(grow.Labels) != 1 || grow.Labels[0] != "ios26" {
		t.Fatalf("grow advice: %+v", resp.Hosts[0].Classes)
	}
}

func TestHintsStripHostLabels(t *testing.T) {
	// Host-pinned demand ("one","ios26") must advise class "ios26" only:
	// daemons never see broker-only host-level labels.
	d1 := newFakeDaemon(t,
		sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"), sim("AAAA-2", "iOS 26.5", "iPhone 17 Pro"),
		sim("AAAA-3", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	for i := 0; i < growColdThreshold; i++ {
		if c, _ := acquire(t, h, "one", "ios26"); c != http.StatusCreated {
			t.Fatalf("acquire %d: %d", i, c)
		}
	}
	hints := b.computeHints(time.Now())
	if len(hints) != 1 || len(hints[0].Classes) != 1 {
		t.Fatalf("hints: %+v", hints)
	}
	got := hints[0].Classes[0].Labels
	if len(got) != 1 || got[0] != "ios26" {
		t.Fatalf("advice labels must be host-label-stripped, got %v", got)
	}
}

func TestNoHintsWhenDemandServedWarm(t *testing.T) {
	d1 := newFakeDaemonOpts(t, fakeOpts{
		status: &proto.HostStatus{Parked: 3, Gates: okGates()},
		parked: func(udid string) bool { return true },
	}, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"), sim("AAAA-2", "iOS 26.5", "iPhone 17 Pro"),
		sim("AAAA-3", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	for i := 0; i < growColdThreshold; i++ {
		if c, _ := acquire(t, h, "ios26"); c != http.StatusCreated {
			t.Fatalf("acquire %d: %d", i, c)
		}
	}
	if hints := b.computeHints(time.Now()); len(hints) != 0 {
		t.Fatalf("warm-served demand must produce no hints: %+v", hints)
	}
}

func TestShrinkHintForIdleWarmPool(t *testing.T) {
	// Host one has parked capacity that never serves a warm hit; host two
	// absorbs all demand cold. one gets shrink advice, two gets grow.
	d1 := newFakeDaemonOpts(t, fakeOpts{
		status: &proto.HostStatus{Parked: 2, Gates: okGates()},
		parked: func(udid string) bool { return true },
	}, sim("AAAA-1", "iOS 18.5", "iPhone 16"))
	d2 := newFakeDaemon(t,
		sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"), sim("BBBB-2", "iOS 26.5", "iPhone 17 Pro"),
		sim("BBBB-3", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})
	h := b.Handler()

	for i := 0; i < growColdThreshold; i++ {
		if c, l := acquire(t, h, "ios26"); c != http.StatusCreated || l.Host != "two" {
			t.Fatalf("acquire %d: %d %+v", i, c, l)
		}
	}

	// A freshly-up host draws no shrink advice: only once it has been up
	// for a full demand window may zero warm hits mean anything.
	if hints := b.computeHints(time.Now()); len(hints) != 1 || hints[0].Host != "two" {
		t.Fatalf("freshly-up host must not draw shrink advice: %+v", hints)
	}
	b.hosts[0].mu.Lock()
	b.hosts[0].upSince = time.Now().Add(-2 * DefaultDemandWindow)
	b.hosts[0].mu.Unlock()

	hints := b.computeHints(time.Now())
	byHost := map[string][]proto.PoolClassAdvice{}
	for _, hh := range hints {
		byHost[hh.Host] = hh.Classes
	}
	if len(byHost["one"]) != 1 || byHost["one"][0].Action != proto.AdviceShrink {
		t.Fatalf("host one advice: %+v", byHost["one"])
	}
	if len(byHost["two"]) != 1 || byHost["two"][0].Action != proto.AdviceGrow {
		t.Fatalf("host two advice: %+v", byHost["two"])
	}
}

func TestAdvisePushFlowsToDaemon(t *testing.T) {
	// End-to-end: cold demand accumulates, a probe round pushes grow
	// advice, and the daemon surfaces it on GET /v0/status.
	d1 := newFakeDaemon(t,
		sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"), sim("AAAA-2", "iOS 26.5", "iPhone 17 Pro"),
		sim("AAAA-3", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	for i := 0; i < growColdThreshold; i++ {
		if c, _ := acquire(t, h, "ios26"); c != http.StatusCreated {
			t.Fatalf("acquire %d: %d", i, c)
		}
	}
	b.probeAll(context.Background())

	resp, err := http.Get(d1.srv.URL + "/v0/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st proto.HostStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.PoolAdvice == nil || st.PoolAdvice.Source != "broker" {
		t.Fatalf("daemon did not record advice: %+v", st.PoolAdvice)
	}
	if len(st.PoolAdvice.Classes) != 1 || st.PoolAdvice.Classes[0].Action != proto.AdviceGrow {
		t.Fatalf("recorded advice: %+v", st.PoolAdvice.Classes)
	}

	// A second round with unchanged hints must not re-push.
	before := st.PoolAdvice.ReceivedAt
	b.probeAll(context.Background())
	resp2, err := http.Get(d1.srv.URL + "/v0/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var st2 proto.HostStatus
	if err := json.NewDecoder(resp2.Body).Decode(&st2); err != nil {
		t.Fatal(err)
	}
	if !st2.PoolAdvice.ReceivedAt.Equal(before) {
		t.Fatalf("unchanged advice was re-pushed: %v != %v", st2.PoolAdvice.ReceivedAt, before)
	}

	// A bounced (down → up) host lost its in-memory advice copy: the
	// next round must re-push even though the hints are unchanged.
	b.hosts[0].mu.Lock()
	b.hosts[0].up = false
	b.hosts[0].mu.Unlock()
	time.Sleep(5 * time.Millisecond) // ensure a later received_at
	b.probeAll(context.Background())
	resp3, err := http.Get(d1.srv.URL + "/v0/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	var st3 proto.HostStatus
	if err := json.NewDecoder(resp3.Body).Decode(&st3); err != nil {
		t.Fatal(err)
	}
	if st3.PoolAdvice == nil || !st3.PoolAdvice.ReceivedAt.After(before) {
		t.Fatalf("advice not re-pushed after host bounce: %+v", st3.PoolAdvice)
	}
}

func TestAdvisePushSkipsOldDaemon(t *testing.T) {
	// A pre-advise daemon (plain 404 on the endpoint) must be marked
	// unsupported and never break probing or placement.
	d1 := newFakeDaemonOpts(t, fakeOpts{noAdvise: true},
		sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"), sim("AAAA-2", "iOS 26.5", "iPhone 17 Pro"),
		sim("AAAA-3", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	for i := 0; i < growColdThreshold; i++ {
		if c, _ := acquire(t, h, "ios26"); c != http.StatusCreated {
			t.Fatalf("acquire %d: %d", i, c)
		}
	}
	b.probeAll(context.Background())
	if !b.hosts[0].adviseUnsupportedNow() {
		t.Fatal("old daemon not marked advise-unsupported")
	}
	// Still healthy and placeable.
	if !b.hosts[0].isUp() {
		t.Fatal("host wrongly marked down")
	}
}
