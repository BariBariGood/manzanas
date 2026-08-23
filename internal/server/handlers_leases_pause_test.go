package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BariBariGood/manzanas/internal/actions/mockapp"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// TestPauseResumeOverHTTP drives the human-preemption flow end-to-end
// over HTTP against the mock backend: acquire → boot → pause (actions
// blocked with lease_paused, observe/screenshot still allowed) →
// resume (actions unblocked).
func TestPauseResumeOverHTTP(t *testing.T) {
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

	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "e2e"}, &l)
	if resp.StatusCode != http.StatusCreated || l.State != proto.LeaseActive {
		t.Fatalf("acquire: %d %+v", resp.StatusCode, l)
	}
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/boot",
		map[string]any{"lease_id": l.ID}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("boot: %d", resp.StatusCode)
	}

	// Pause with a reason.
	var paused proto.Lease
	resp = doJSON(t, "POST", ts.URL+"/v0/leases/"+l.ID+"/pause",
		proto.PauseLeaseRequest{Reason: "human-present"}, &paused)
	if resp.StatusCode != http.StatusOK || !paused.Paused ||
		paused.PausedAt == nil || paused.PausedReason != "human-present" {
		t.Fatalf("pause: %d %+v", resp.StatusCode, paused)
	}
	if paused.State != proto.LeaseActive {
		t.Fatalf("paused lease must stay active, got %+v", paused)
	}

	// Mutating actions are refused with lease_paused.
	var wireErr proto.Error
	resp = doJSON(t, "POST", ts.URL+"/v0/actions",
		proto.ActionRequest{LeaseID: l.ID, Kind: "tap", Payload: map[string]any{"x": 1, "y": 2}}, &wireErr)
	if resp.StatusCode != http.StatusConflict || wireErr.Code != proto.ErrLeasePaused {
		t.Fatalf("paused tap: %d %+v, want 409 lease_paused", resp.StatusCode, wireErr)
	}

	// Read-only observation stays available.
	var obs proto.ActionResult
	resp = doJSON(t, "POST", ts.URL+"/v0/actions",
		proto.ActionRequest{LeaseID: l.ID, Kind: "observe"}, &obs)
	if resp.StatusCode != http.StatusOK || !obs.OK {
		t.Fatalf("paused observe: %d %+v", resp.StatusCode, obs)
	}
	var shot proto.ActionResult
	resp = doJSON(t, "POST", ts.URL+"/v0/actions",
		proto.ActionRequest{LeaseID: l.ID, Kind: "screenshot"}, &shot)
	if resp.StatusCode != http.StatusOK || !shot.OK {
		t.Fatalf("paused screenshot: %d %+v", resp.StatusCode, shot)
	}

	// Renewal keeps working (and keeps the pause).
	var renewed proto.Lease
	resp = doJSON(t, "POST", ts.URL+"/v0/leases/"+l.ID+"/renew",
		proto.RenewLeaseRequest{TTLSeconds: 120}, &renewed)
	if resp.StatusCode != http.StatusOK || !renewed.Paused {
		t.Fatalf("renew while paused: %d %+v", resp.StatusCode, renewed)
	}

	// Resume: pause fields cleared, actions unblocked.
	var resumed proto.Lease
	resp = doJSON(t, "POST", ts.URL+"/v0/leases/"+l.ID+"/resume", nil, &resumed)
	if resp.StatusCode != http.StatusOK || resumed.Paused ||
		resumed.PausedAt != nil || resumed.PausedReason != "" {
		t.Fatalf("resume: %d %+v", resp.StatusCode, resumed)
	}
	var tap proto.ActionResult
	resp = doJSON(t, "POST", ts.URL+"/v0/actions",
		proto.ActionRequest{LeaseID: l.ID, Kind: "tap", Payload: map[string]any{"x": 1, "y": 2}}, &tap)
	if resp.StatusCode != http.StatusOK || !tap.OK {
		t.Fatalf("post-resume tap: %d %+v", resp.StatusCode, tap)
	}

	// Pause/resume on unknown and terminal leases.
	resp = doJSON(t, "POST", ts.URL+"/v0/leases/lse_nope/pause", nil, &wireErr)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pause unknown: %d", resp.StatusCode)
	}
	resp = doJSON(t, "DELETE", ts.URL+"/v0/leases/"+l.ID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release: %d", resp.StatusCode)
	}
	resp = doJSON(t, "POST", ts.URL+"/v0/leases/"+l.ID+"/pause", nil, &wireErr)
	if resp.StatusCode != http.StatusGone || wireErr.Code != proto.ErrLeaseExpired {
		t.Fatalf("pause released: %d %+v, want 410 lease_expired", resp.StatusCode, wireErr)
	}
	resp = doJSON(t, "POST", ts.URL+"/v0/leases/"+l.ID+"/resume", nil, &wireErr)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("resume released: %d", resp.StatusCode)
	}
}
