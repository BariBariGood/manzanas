package server

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BariBariGood/manzanas/internal/actions/mockapp"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// TestMockBackendFullLoopOverHTTP drives the whole agent loop against the
// mock action backend exactly as `manzanasd --mock` wires it:
// lease → boot → observe → tap_element → type_into_element →
// wait_for_element → audit → screenshot → release. This is the CI-side
// proof the full action surface works off-Mac with no simulator.
func TestMockBackendFullLoopOverHTTP(t *testing.T) {
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	store := mockapp.NewStore()
	srv.SetActions(mockapp.NewBackend(store, mockapp.WithBooted(func(ctx context.Context, udid string) bool {
		st, err := reg.Health(ctx, udid)
		return err == nil && st == proto.StateBooted
	})))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Lease a mock target.
	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "e2e"}, &l)
	if resp.StatusCode != http.StatusCreated || l.State != proto.LeaseActive {
		t.Fatalf("acquire: %d %+v", resp.StatusCode, l)
	}
	act := func(kind string, payload map[string]any) proto.ActionResult {
		t.Helper()
		var res proto.ActionResult
		resp := doJSON(t, "POST", ts.URL+"/v0/actions",
			proto.ActionRequest{LeaseID: l.ID, Kind: kind, Payload: payload}, &res)
		if resp.StatusCode != http.StatusOK || !res.OK {
			t.Fatalf("%s: %d %+v", kind, resp.StatusCode, res)
		}
		return res
	}

	// Actions against a shutdown target surface target_not_booted.
	var wireErr struct {
		Code string `json:"code"`
	}
	resp = doJSON(t, "POST", ts.URL+"/v0/actions",
		proto.ActionRequest{LeaseID: l.ID, Kind: "tap", Payload: map[string]any{"x": 1, "y": 2}}, &wireErr)
	if resp.StatusCode != http.StatusConflict || wireErr.Code != proto.ErrTargetNotBooted {
		t.Fatalf("pre-boot tap: %d %+v, want 409 target_not_booted", resp.StatusCode, wireErr)
	}

	// Boot, then run the loop.
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/boot",
		map[string]any{"lease_id": l.ID}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("boot: %d", resp.StatusCode)
	}

	obs := act("observe", nil)
	if obs.Result["hash"] == "" || obs.Result["tree"] == nil {
		t.Fatalf("observe: %+v", obs.Result)
	}
	act("tap_element", map[string]any{"id": "username"})
	act("type_into_element", map[string]any{"id": "username", "text": "agent", "require_focus": true})
	act("type_into_element", map[string]any{"id": "password", "text": "pw"})
	act("tap_element", map[string]any{"label": "Sign In"})
	wf := act("wait_for_element", map[string]any{"label": "Welcome, agent!", "timeout_ms": 5000})
	if wf.Result["element"] == nil {
		t.Fatal("wait_for_element should return the welcome label")
	}

	audit := act("audit", nil)
	if audit.Result["findings"] == nil || audit.Result["counts"] == nil {
		t.Fatalf("audit: %+v", audit.Result)
	}

	shot := act("screenshot", nil)
	b64, _ := shot.Result["png_base64"].(string)
	img, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(img) < 8 || string(img[1:4]) != "PNG" {
		t.Fatalf("screenshot did not return a PNG (%d bytes, err %v)", len(img), err)
	}

	// Batch form works over the same backend.
	var batch proto.BatchActionResult
	resp = doJSON(t, "POST", ts.URL+"/v0/actions:batch", proto.BatchActionRequest{
		LeaseID: l.ID, StopOnError: true, Actions: []proto.BatchAction{
			{Kind: "tap_element", Payload: map[string]any{"label": "Reset"}},
			{Kind: "wait_for_element", Payload: map[string]any{"label": "Welcome, agent!", "absent": true, "timeout_ms": 5000}},
		}}, &batch)
	if resp.StatusCode != http.StatusOK || !batch.OK || batch.Completed != 2 {
		t.Fatalf("batch: %d %+v", resp.StatusCode, batch)
	}

	// Release the lease.
	resp = doJSON(t, "DELETE", ts.URL+"/v0/leases/"+l.ID, nil, nil)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("release: %d", resp.StatusCode)
	}
}
