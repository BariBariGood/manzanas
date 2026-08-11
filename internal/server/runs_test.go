package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions/mockapp"
	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// newRunTestServer wires a server exactly like `manzanasd --mock` (mock
// registry + mock action backend), with the journal in a temp dir.
func newRunTestServer(t *testing.T) *httptest.Server {
	t.Helper()
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
	js, err := journal.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv.SetJournal(journal.NewRecorder(js))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestRunSyncFullLoop drives the whole one-call loop synchronously against
// the mock backend: lease → boot → install → launch → element steps →
// batch → final screenshot → release, with journal export attached.
func TestRunSyncFullLoop(t *testing.T) {
	ts := newRunTestServer(t)
	req := proto.RunRequest{
		AgentID: "run-test",
		Spec: proto.RunSpec{
			Name:   "login-smoke",
			Target: proto.RunTarget{Labels: []string{"ios26"}},
			App:    &proto.RunApp{Path: "/tmp/Fake.app", BundleID: "com.example.fake"},
			Steps: []proto.RunStep{
				{Name: "focus username", Action: "tap_element", With: map[string]any{"id": "username"}},
				{Action: "type_into_element", With: map[string]any{"id": "username", "text": "agent"}},
				{Action: "type_into_element", With: map[string]any{"id": "password", "text": "pw"}},
				{Action: "tap_element", With: map[string]any{"label": "Sign In"}},
				{Action: "wait_for_element", With: map[string]any{"label": "Welcome, agent!", "timeout_ms": 5000}},
				{Action: "audit"},
				{Action: "screenshot"},
				{Action: "batch", With: map[string]any{"actions": []any{
					map[string]any{"kind": "tap_element", "payload": map[string]any{"label": "Reset"}},
					map[string]any{"kind": "wait_for_element", "payload": map[string]any{"label": "Welcome, agent!", "absent": true, "timeout_ms": 5000}},
				}}},
			},
			Timeouts: proto.RunTimeouts{RunSeconds: 60},
		},
	}
	var run proto.Run
	resp := doJSON(t, "POST", ts.URL+"/v0/runs", req, &run)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v0/runs: %d %+v", resp.StatusCode, run)
	}
	if run.State != proto.RunPassed {
		t.Fatalf("run state %s (error %+v, steps %+v)", run.State, run.Error, run.Steps)
	}
	if run.LeaseID == "" || run.TargetUDID == "" {
		t.Fatalf("missing lease/target: %+v", run)
	}
	if run.Stage != "" {
		t.Fatalf("passed run should have no stage, got %q", run.Stage)
	}
	if len(run.Steps) != len(req.Spec.Steps) {
		t.Fatalf("got %d step results, want %d", len(run.Steps), len(req.Spec.Steps))
	}
	for _, st := range run.Steps {
		if st.Status != proto.StepOK {
			t.Fatalf("step %d %s: %s (%+v)", st.Index, st.Action, st.Status, st.Error)
		}
	}
	if run.ExportMD == "" || !strings.Contains(run.ExportMD, run.LeaseID) {
		t.Fatalf("export_md missing or unlinked (len %d)", len(run.ExportMD))
	}
	// The lease must be gone (released by the run).
	resp = doJSON(t, "GET", ts.URL+"/v0/leases/"+run.LeaseID, nil, nil)
	if resp.StatusCode == http.StatusOK {
		var l proto.Lease
		doJSON(t, "GET", ts.URL+"/v0/leases/"+run.LeaseID, nil, &l)
		if l.State == proto.LeaseActive {
			t.Fatalf("lease still active after run: %+v", l)
		}
	}
	// GET /v0/runs/{id} serves the same resource.
	var got proto.Run
	resp = doJSON(t, "GET", ts.URL+"/v0/runs/"+run.ID, nil, &got)
	if resp.StatusCode != http.StatusOK || got.State != proto.RunPassed {
		t.Fatalf("GET run: %d %+v", resp.StatusCode, got)
	}
}

// TestRunContinueOnErrorFailureDoesNotHalt proves a continue_on_error
// step's own failure fails the run but does not stop later steps.
func TestRunContinueOnErrorFailureDoesNotHalt(t *testing.T) {
	ts := newRunTestServer(t)
	req := proto.RunRequest{
		AgentID: "run-test",
		Spec: proto.RunSpec{
			Target: proto.RunTarget{Labels: []string{"ios26"}},
			Steps: []proto.RunStep{
				{Action: "tap_element", With: map[string]any{"id": "does-not-exist", "timeout_ms": 300}, ContinueOnError: true},
				{Action: "screenshot"},
			},
			Timeouts: proto.RunTimeouts{RunSeconds: 60},
		},
	}
	var run proto.Run
	resp := doJSON(t, "POST", ts.URL+"/v0/runs", req, &run)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v0/runs: %d", resp.StatusCode)
	}
	if run.State != proto.RunFailed || run.Error == nil {
		t.Fatalf("want failed run, got %s %+v", run.State, run.Error)
	}
	if run.Steps[0].Status != proto.StepError {
		t.Fatalf("step 0: %+v", run.Steps[0])
	}
	if run.Steps[1].Status != proto.StepOK {
		t.Fatalf("step 1 should still run after a continue_on_error failure: %+v", run.Steps[1])
	}
}

// TestRunStepFailureReleasesLease proves a failing step fails the run,
// skips later steps, and still releases the lease and exports evidence.
func TestRunStepFailureReleasesLease(t *testing.T) {
	ts := newRunTestServer(t)
	req := proto.RunRequest{
		AgentID: "run-test",
		Spec: proto.RunSpec{
			Target: proto.RunTarget{Labels: []string{"ios26"}},
			Steps: []proto.RunStep{
				{Action: "tap_element", With: map[string]any{"id": "does-not-exist", "timeout_ms": 300}},
				{Action: "screenshot"},
				{Action: "audit", ContinueOnError: true},
			},
			Timeouts: proto.RunTimeouts{RunSeconds: 60},
		},
	}
	var run proto.Run
	resp := doJSON(t, "POST", ts.URL+"/v0/runs", req, &run)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v0/runs: %d", resp.StatusCode)
	}
	if run.State != proto.RunFailed || run.Error == nil {
		t.Fatalf("want failed run, got %s %+v", run.State, run.Error)
	}
	if run.Stage != "steps" {
		t.Fatalf("failed run should keep the failing stage, got %q", run.Stage)
	}
	if run.Steps[0].Status != proto.StepError {
		t.Fatalf("step 0: %+v", run.Steps[0])
	}
	if run.Steps[1].Status != proto.StepSkipped {
		t.Fatalf("step 1 should be skipped: %+v", run.Steps[1])
	}
	if run.Steps[2].Status != proto.StepOK {
		t.Fatalf("step 2 has continue_on_error, should still run: %+v", run.Steps[2])
	}
	if run.ExportMD == "" {
		t.Fatal("failed runs must still export evidence")
	}
	var l proto.Lease
	resp = doJSON(t, "GET", ts.URL+"/v0/leases/"+run.LeaseID, nil, &l)
	if resp.StatusCode == http.StatusOK && l.State == proto.LeaseActive {
		t.Fatalf("lease still active after failed run: %+v", l)
	}
}

// TestRunAsync exercises async submission + polling.
func TestRunAsync(t *testing.T) {
	ts := newRunTestServer(t)
	req := proto.RunRequest{
		AgentID: "run-test",
		Async:   true,
		Spec: proto.RunSpec{
			Target:   proto.RunTarget{Labels: []string{"ios26"}},
			Steps:    []proto.RunStep{{Action: "observe"}},
			Timeouts: proto.RunTimeouts{RunSeconds: 60},
		},
	}
	var run proto.Run
	resp := doJSON(t, "POST", ts.URL+"/v0/runs", req, &run)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("async POST: %d", resp.StatusCode)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var got proto.Run
		resp = doJSON(t, "GET", ts.URL+"/v0/runs/"+run.ID, nil, &got)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET run: %d", resp.StatusCode)
		}
		if got.State == proto.RunPassed {
			break
		}
		if got.State == proto.RunFailed {
			t.Fatalf("async run failed: %+v", got.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("async run did not finish (state %s, stage %s)", got.State, got.Stage)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Listing includes it (without the bulky export).
	var list proto.RunList
	resp = doJSON(t, "GET", ts.URL+"/v0/runs", nil, &list)
	if resp.StatusCode != http.StatusOK || len(list.Runs) == 0 || list.Runs[0].ExportMD != "" {
		t.Fatalf("list: %d %+v", resp.StatusCode, list)
	}
}

// TestRunValidation covers request-level rejections.
func TestRunValidation(t *testing.T) {
	ts := newRunTestServer(t)
	cases := []struct {
		name   string
		req    proto.RunRequest
		status int
		code   string
	}{
		{"missing agent", proto.RunRequest{Spec: proto.RunSpec{Target: proto.RunTarget{Labels: []string{"a"}}}},
			http.StatusBadRequest, proto.ErrBadRequest},
		{"no target", proto.RunRequest{AgentID: "t"}, http.StatusBadRequest, proto.ErrBadRequest},
		{"golden image reserved", proto.RunRequest{AgentID: "t",
			Spec: proto.RunSpec{Target: proto.RunTarget{Labels: []string{"a"}, Image: "golden"}}},
			http.StatusNotImplemented, proto.ErrNotImplemented},
		{"maestro reserved", proto.RunRequest{AgentID: "t",
			Spec: proto.RunSpec{Target: proto.RunTarget{Labels: []string{"a"}},
				Steps: []proto.RunStep{{MaestroFlow: "f.yaml"}}}},
			http.StatusNotImplemented, proto.ErrNotImplemented},
		{"no matching target", proto.RunRequest{AgentID: "t",
			Spec: proto.RunSpec{Target: proto.RunTarget{Labels: []string{"no-such-label"}}}},
			http.StatusOK, proto.ErrNoMatch}, // sync run finishes failed
	}
	for _, tc := range cases {
		var body struct {
			Code  string       `json:"code"`
			State string       `json:"state"`
			Error *proto.Error `json:"error"`
		}
		resp := doJSON(t, "POST", ts.URL+"/v0/runs", tc.req, &body)
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.status)
			continue
		}
		code := body.Code
		if resp.StatusCode == http.StatusOK {
			if body.State != string(proto.RunFailed) || body.Error == nil {
				t.Errorf("%s: want failed run, got %+v", tc.name, body)
				continue
			}
			code = body.Error.Code
		}
		if code != tc.code {
			t.Errorf("%s: code %q, want %q", tc.name, code, tc.code)
		}
	}
}

func TestRunTargetLabels(t *testing.T) {
	got := runTargetLabels(proto.RunTarget{
		Labels: []string{"ios26"}, Runtime: "iOS 26.5", DeviceType: "iPhone 17 Pro"})
	want := []string{"ios26", "ios26.5", "iphone-17-pro"}
	if len(got) != len(want) {
		t.Fatalf("labels %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("labels %v, want %v", got, want)
		}
	}
}
