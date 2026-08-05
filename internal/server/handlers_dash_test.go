package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// newDashTestServer wires a mock-registry server and returns the Server so
// tests can flip dash gates (readonly, parked).
func newDashTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

func firstTarget(t *testing.T, ts *httptest.Server) proto.Target {
	t.Helper()
	var list struct {
		Targets []proto.Target `json:"targets"`
	}
	resp := doJSON(t, "GET", ts.URL+"/v0/targets", nil, &list)
	if resp.StatusCode != 200 || len(list.Targets) == 0 {
		t.Fatalf("targets: %d %+v", resp.StatusCode, list)
	}
	return list.Targets[0]
}

func TestDashConfig(t *testing.T) {
	ts, srv := newDashTestServer(t)

	var cfg struct {
		Readonly bool `json:"readonly"`
	}
	resp := doJSON(t, "GET", ts.URL+"/v0/dash/config", nil, &cfg)
	if resp.StatusCode != 200 || cfg.Readonly {
		t.Fatalf("config: %d %+v, want readonly=false", resp.StatusCode, cfg)
	}

	srv.SetDashReadonly(true)
	resp = doJSON(t, "GET", ts.URL+"/v0/dash/config", nil, &cfg)
	if resp.StatusCode != 200 || !cfg.Readonly {
		t.Fatalf("config: %d %+v, want readonly=true", resp.StatusCode, cfg)
	}
}

func TestDashBootShutdownFreeTarget(t *testing.T) {
	ts, _ := newDashTestServer(t)
	tgt := firstTarget(t, ts)

	var booted proto.Target
	resp := doJSON(t, "POST", ts.URL+"/v0/dash/targets/"+tgt.UDID+"/boot", nil, &booted)
	if resp.StatusCode != http.StatusAccepted || booted.State != proto.StateBooted {
		t.Fatalf("dash boot: %d %+v", resp.StatusCode, booted)
	}

	var shut proto.Target
	resp = doJSON(t, "POST", ts.URL+"/v0/dash/targets/"+tgt.UDID+"/shutdown", nil, &shut)
	if resp.StatusCode != http.StatusAccepted || shut.State != proto.StateShutdown {
		t.Fatalf("dash shutdown: %d %+v", resp.StatusCode, shut)
	}
}

func TestDashOpsRefuseLeasedTarget(t *testing.T) {
	ts, _ := newDashTestServer(t)
	tgt := firstTarget(t, ts)

	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{UDID: tgt.UDID, AgentID: "agent-1"}, &l)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("acquire: %d", resp.StatusCode)
	}

	for _, op := range []string{"boot", "shutdown"} {
		var e proto.Error
		resp = doJSON(t, "POST", ts.URL+"/v0/dash/targets/"+tgt.UDID+"/"+op, nil, &e)
		if resp.StatusCode != http.StatusConflict || e.Code != proto.ErrTargetBusy {
			t.Errorf("dash %s on leased target: %d %+v, want 409 target_busy", op, resp.StatusCode, e)
		}
	}
}

func TestDashOpsRefuseSentinelHeldTarget(t *testing.T) {
	ts, srv := newDashTestServer(t)
	tgt := firstTarget(t, ts)

	// A warm-pool Reserve holds the target without an active lease
	// (same byTarget sentinel mechanism as a post-lease reset).
	if _, ok := srv.leases.Reserve(tgt.UDID); !ok {
		t.Fatalf("reserve %s failed", tgt.UDID)
	}
	defer srv.leases.Unreserve(tgt.UDID)

	for _, op := range []string{"boot", "shutdown"} {
		var e proto.Error
		resp := doJSON(t, "POST", ts.URL+"/v0/dash/targets/"+tgt.UDID+"/"+op, nil, &e)
		if resp.StatusCode != http.StatusConflict || e.Code != proto.ErrTargetBusy {
			t.Errorf("dash %s on reserved target: %d %+v, want 409 target_busy", op, resp.StatusCode, e)
		}
	}
}

func TestDashRefusesParkedTarget(t *testing.T) {
	ts, srv := newDashTestServer(t)
	tgt := firstTarget(t, ts)
	srv.SetParkedCheck(func(udid string) bool { return udid == tgt.UDID })

	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/dash/targets/"+tgt.UDID+"/boot", nil, &e)
	if resp.StatusCode != http.StatusConflict || e.Code != proto.ErrTargetBusy {
		t.Fatalf("dash boot on parked target: %d %+v, want 409 target_busy", resp.StatusCode, e)
	}
}

func TestDashReleaseLease(t *testing.T) {
	ts, _ := newDashTestServer(t)
	tgt := firstTarget(t, ts)

	// No active lease: 404.
	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/dash/targets/"+tgt.UDID+"/release", nil, &e)
	if resp.StatusCode != http.StatusNotFound || e.Code != proto.ErrNotFound {
		t.Fatalf("dash release without lease: %d %+v, want 404 not_found", resp.StatusCode, e)
	}

	var l proto.Lease
	resp = doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{UDID: tgt.UDID, AgentID: "agent-1"}, &l)
	if resp.StatusCode != http.StatusCreated || l.State != proto.LeaseActive {
		t.Fatalf("acquire: %d %+v", resp.StatusCode, l)
	}

	var released proto.Lease
	resp = doJSON(t, "POST", ts.URL+"/v0/dash/targets/"+tgt.UDID+"/release", nil, &released)
	if resp.StatusCode != 200 || released.State != proto.LeaseReleased {
		t.Fatalf("dash release: %d %+v", resp.StatusCode, released)
	}
	// The dashboard never sees capability tokens.
	if released.ID != "" {
		t.Errorf("dash release leaked lease ID %q", released.ID)
	}

	var got proto.Lease
	resp = doJSON(t, "GET", ts.URL+"/v0/leases/"+l.ID, nil, &got)
	if resp.StatusCode != 200 || got.State != proto.LeaseReleased {
		t.Fatalf("lease after dash release: %d %+v, want released", resp.StatusCode, got)
	}
}

func TestDashReadonlyRefusesMutations(t *testing.T) {
	ts, srv := newDashTestServer(t)
	tgt := firstTarget(t, ts)
	srv.SetDashReadonly(true)

	for _, op := range []string{"boot", "shutdown", "release", "recording/stop"} {
		var e proto.Error
		resp := doJSON(t, "POST", ts.URL+"/v0/dash/targets/"+tgt.UDID+"/"+op, nil, &e)
		// recording/stop answers 501 before the gate when recording is not
		// wired at all (capability probing wins over the readonly refusal).
		if op == "recording/stop" {
			if resp.StatusCode != http.StatusNotImplemented {
				t.Errorf("dash %s readonly: %d, want 501", op, resp.StatusCode)
			}
			continue
		}
		if resp.StatusCode != http.StatusForbidden || e.Code != proto.ErrReadOnly {
			t.Errorf("dash %s readonly: %d %+v, want 403 read_only", op, resp.StatusCode, e)
		}
	}
}

func TestDashRecordingStop(t *testing.T) {
	ts, _ := newRecordingTestServer(t)
	l := acquireAndBoot(t, ts, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "rec-test"})

	// No live recording yet: 404.
	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/dash/targets/"+l.TargetUDID+"/recording/stop", nil, &e)
	if resp.StatusCode != http.StatusNotFound || e.Code != proto.ErrNotFound {
		t.Fatalf("dash recording stop without recording: %d %+v, want 404", resp.StatusCode, e)
	}

	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/recording",
		proto.RecordingRequest{LeaseID: l.ID}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("recording start: %d", resp.StatusCode)
	}

	// The targets listing marks the live recording.
	var list struct {
		Targets []proto.Target `json:"targets"`
	}
	resp = doJSON(t, "GET", ts.URL+"/v0/targets", nil, &list)
	if resp.StatusCode != 200 {
		t.Fatalf("targets: %d", resp.StatusCode)
	}
	for _, tgt := range list.Targets {
		if want := tgt.UDID == l.TargetUDID; tgt.Recording != want {
			t.Errorf("target %s recording=%v, want %v", tgt.UDID, tgt.Recording, want)
		}
	}

	var out proto.RecordingStopResult
	resp = doJSON(t, "POST", ts.URL+"/v0/dash/targets/"+l.TargetUDID+"/recording/stop", nil, &out)
	if resp.StatusCode != 200 || !out.OK || out.Reason != "dash_stop" {
		t.Fatalf("dash recording stop: %d %+v", resp.StatusCode, out)
	}
}
