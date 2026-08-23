package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/BariBariGood/manzanas/internal/actions"
	"github.com/BariBariGood/manzanas/internal/actions/mockapp"
	"github.com/BariBariGood/manzanas/internal/elementmap"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// observeCountingRunner wraps the mock runner and counts describe-ui
// (observe) invocations, so the test can prove the map rung serves a
// cached frame with NO observe round-trip.
type observeCountingRunner struct {
	inner    *mockapp.Runner
	observes atomic.Int64
}

func (r *observeCountingRunner) count(args []string) {
	if len(args) > 0 && args[0] == "describe-ui" {
		r.observes.Add(1)
	}
}

func (r *observeCountingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	r.count(args)
	return r.inner.Run(ctx, name, args...)
}

func (r *observeCountingRunner) RunInput(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	r.count(args)
	return r.inner.RunInput(ctx, stdin, name, args...)
}

// TestElementMapEndToEndOverHTTP drives the element-map fast path
// exactly as `manzanasd --mock --element-map` wires it: the first
// tap_element resolves through the a11y rung (resolver:"ax") and learns
// the element; the second resolves through the map rung
// (resolver:"map") with zero additional observes; the CLI-facing
// GET /v0/elementmap and DELETE endpoints see (and clear) the learned
// entry, after which resolution goes back through ax.
func TestElementMapEndToEndOverHTTP(t *testing.T) {
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	emap, err := elementmap.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv.SetElementMap(emap)
	store := mockapp.NewStore()
	runner := &observeCountingRunner{inner: mockapp.NewRunner(store, mockapp.WithBooted(func(ctx context.Context, udid string) bool {
		st, err := reg.Health(ctx, udid)
		return err == nil && st == proto.StateBooted
	}))}
	backend := actions.NewAXe(actions.WithRunner(runner), actions.WithAXePath(mockapp.AXeBin))
	backend.SetElementMapStore(emap)
	srv.SetActions(backend)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "emap-e2e"}, &l)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("acquire: %d", resp.StatusCode)
	}
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/boot",
		map[string]any{"lease_id": l.ID}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("boot: %d", resp.StatusCode)
	}
	var res proto.ActionResult
	resp = doJSON(t, "POST", ts.URL+"/v0/actions", proto.ActionRequest{
		LeaseID: l.ID, Kind: "launch_app",
		Payload: map[string]any{"bundle_id": "com.example.mock"}}, &res)
	if resp.StatusCode != http.StatusOK || !res.OK {
		t.Fatalf("launch_app: %d %+v", resp.StatusCode, res)
	}

	tap := func() proto.ActionResult {
		t.Helper()
		var res proto.ActionResult
		resp := doJSON(t, "POST", ts.URL+"/v0/actions", proto.ActionRequest{
			LeaseID: l.ID, Kind: "tap_element",
			Payload: map[string]any{"label": "Reset"}}, &res)
		if resp.StatusCode != http.StatusOK || !res.OK {
			t.Fatalf("tap_element: %d %+v", resp.StatusCode, res)
		}
		return res
	}

	// First tap: the map is empty, so the a11y rung resolves (and the
	// element is learned).
	first := tap()
	if got, _ := first.Result["resolver"].(string); got != "ax" {
		t.Fatalf("first tap resolver = %q, want \"ax\" (%+v)", got, first.Result)
	}
	afterFirst := runner.observes.Load()
	if afterFirst == 0 {
		t.Fatal("first tap performed no observe")
	}

	var list struct {
		Elements []elementmap.Entry `json:"elements"`
	}
	resp = doJSON(t, "GET", ts.URL+"/v0/elementmap", nil, &list)
	if resp.StatusCode != http.StatusOK || len(list.Elements) != 1 {
		t.Fatalf("list: %d %+v", resp.StatusCode, list)
	}
	e := list.Elements[0]
	if e.BundleID != "com.example.mock" || e.Name != `label="Reset"` || e.Element.Frame.W <= 0 {
		t.Fatalf("bad learned entry: %+v", e)
	}

	// Second tap: the map rung serves the cached frame — provenance
	// says "map" and the observe count is unchanged (no round-trip).
	second := tap()
	if got, _ := second.Result["resolver"].(string); got != "map" {
		t.Fatalf("second tap resolver = %q, want \"map\" (%+v)", got, second.Result)
	}
	if n := runner.observes.Load(); n != afterFirst {
		t.Fatalf("map-rung tap performed %d observe(s)", n-afterFirst)
	}

	var del struct {
		Removed int `json:"removed"`
	}
	resp = doJSON(t, "DELETE", ts.URL+"/v0/elementmap/com.example.mock?name="+
		url.QueryEscape(`label="Reset"`), nil, &del)
	if resp.StatusCode != http.StatusOK || del.Removed != 1 {
		t.Fatalf("delete: %d %+v", resp.StatusCode, del)
	}

	// With the entry gone, resolution goes back through ax (and
	// re-learns).
	third := tap()
	if got, _ := third.Result["resolver"].(string); got != "ax" {
		t.Fatalf("third tap resolver = %q, want \"ax\" (%+v)", got, third.Result)
	}
}
