package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/record"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// newRecordingTestServer wires a mock registry, journal, and a recorder
// manager whose children are real MockStarter shell processes.
func newRecordingTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	dir := t.TempDir()
	store, err := journal.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetJournal(journal.NewRecorder(store))
	srv.SetRecorderManager(record.NewManager(record.Config{
		Starter:     record.MockStarter{},
		JournalRoot: dir,
		StartProbe:  50 * time.Millisecond,
		ReapTimeout: 5 * time.Second,
	}))
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

func acquireAndBoot(t *testing.T, ts *httptest.Server, req proto.AcquireLeaseRequest) proto.Lease {
	t.Helper()
	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases", req, &l)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("acquire: %d", resp.StatusCode)
	}
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/boot",
		map[string]any{"lease_id": l.ID}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("boot: %d", resp.StatusCode)
	}
	return l
}

func journalReasons(t *testing.T, ts *httptest.Server, runID string) map[string]bool {
	t.Helper()
	var page struct {
		Entries []journal.Entry `json:"entries"`
	}
	resp := doJSON(t, "GET", ts.URL+"/v0/journal/"+runID, nil, &page)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("journal get: %d", resp.StatusCode)
	}
	reasons := map[string]bool{}
	for _, e := range page.Entries {
		if e.Kind != "segment" {
			continue
		}
		if params, ok := e.Payload["params"].(map[string]any); ok {
			if r, ok := params["reason"].(string); ok {
				reasons[r] = true
			}
		}
	}
	return reasons
}

func TestRecordingNotWiredReturns501(t *testing.T) {
	ts := newTestServer(t)
	resp := doJSON(t, "POST", ts.URL+"/v0/targets/x/recording", nil, nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

func TestRecordingLifecycle(t *testing.T) {
	ts, _ := newRecordingTestServer(t)
	l := acquireAndBoot(t, ts, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "rec-test"})

	var rec proto.Recording
	resp := doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/recording",
		proto.RecordingRequest{LeaseID: l.ID}, &rec)
	if resp.StatusCode != http.StatusCreated || rec.ID == "" || rec.Codec != "hevc" {
		t.Fatalf("start: %d %+v", resp.StatusCode, rec)
	}

	// A second start on the same target conflicts.
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/recording",
		proto.RecordingRequest{LeaseID: l.ID}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate start: %d, want 409", resp.StatusCode)
	}

	// Let the mock child install its trap before stopping.
	time.Sleep(300 * time.Millisecond)
	var out proto.RecordingStopResult
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/recording/stop",
		proto.RecordingStopRequest{LeaseID: l.ID}, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop: %d", resp.StatusCode)
	}
	if !out.OK || out.Artifact == nil || out.JournalRef == nil ||
		out.Reason != "stopped" || out.Bytes == 0 {
		t.Fatalf("stop result = %+v", out)
	}

	// The artifact downloads through the journal artifact GET.
	get, err := http.Get(ts.URL + "/v0/journal/" + l.ID + "/artifacts/" + out.Artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("artifact fetch: %d", get.StatusCode)
	}

	if !journalReasons(t, ts, l.ID)["stopped"] {
		t.Fatal("no segment entry with reason=stopped")
	}
}

func TestRecordingRequiresBootedTarget(t *testing.T) {
	ts, _ := newRecordingTestServer(t)
	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "rec-test"}, &l)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("acquire: %d", resp.StatusCode)
	}
	// Target is Shutdown: start must refuse (recordVideo would "succeed"
	// with a 0-byte file).
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/recording",
		proto.RecordingRequest{LeaseID: l.ID}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("start on shutdown target: %d, want 409", resp.StatusCode)
	}
}

func TestRecordingAutoStopOnRelease(t *testing.T) {
	ts, _ := newRecordingTestServer(t)
	l := acquireAndBoot(t, ts, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "rec-test"})

	resp := doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/recording",
		proto.RecordingRequest{LeaseID: l.ID}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	time.Sleep(300 * time.Millisecond)

	// Release without stopping: the daemon must stop + ingest with
	// reason lease_end.
	resp = doJSON(t, "DELETE", ts.URL+"/v0/leases/"+l.ID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release: %d", resp.StatusCode)
	}
	waitForCond(t, 10*time.Second, func() bool {
		return journalReasons(t, ts, l.ID)["lease_end"]
	})
}

func TestRecordingAutoStopOnShutdown(t *testing.T) {
	ts, _ := newRecordingTestServer(t)
	l := acquireAndBoot(t, ts, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "rec-test"})

	resp := doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/recording",
		proto.RecordingRequest{LeaseID: l.ID}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	time.Sleep(300 * time.Millisecond)

	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/shutdown",
		map[string]any{"lease_id": l.ID}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("shutdown: %d", resp.StatusCode)
	}
	if !journalReasons(t, ts, l.ID)["target_shutdown"] {
		t.Fatal("no segment entry with reason=target_shutdown")
	}
}

func TestAutoRecordLease(t *testing.T) {
	ts, srv := newRecordingTestServer(t)
	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "rec-test", Record: "video"}, &l)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("acquire: %d", resp.StatusCode)
	}
	if l.Record != "video" {
		t.Fatalf("lease.record = %q", l.Record)
	}
	// Not recording yet: target is Shutdown.
	if srv.recorder.Recording(l.TargetUDID) {
		t.Fatal("recording before boot")
	}
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/boot",
		map[string]any{"lease_id": l.ID}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("boot: %d", resp.StatusCode)
	}
	// Auto-record starts off the request goroutine.
	waitForCond(t, 10*time.Second, func() bool {
		return srv.recorder.Recording(l.TargetUDID)
	})
	time.Sleep(300 * time.Millisecond)
	resp = doJSON(t, "DELETE", ts.URL+"/v0/leases/"+l.ID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release: %d", resp.StatusCode)
	}
	waitForCond(t, 10*time.Second, func() bool {
		return journalReasons(t, ts, l.ID)["lease_end"]
	})
}

func TestAutoRecordStartsAfterAsyncBoot(t *testing.T) {
	old := autoRecordPollInterval
	autoRecordPollInterval = 50 * time.Millisecond
	t.Cleanup(func() { autoRecordPollInterval = old })

	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	dir := t.TempDir()
	store, err := journal.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetJournal(journal.NewRecorder(store))
	srv.SetRecorderManager(record.NewManager(record.Config{
		Starter:     record.MockStarter{},
		JournalRoot: dir,
		StartProbe:  50 * time.Millisecond,
		ReapTimeout: 5 * time.Second,
	}))
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "rec-test", Record: "video"}, &l)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("acquire: %d", resp.StatusCode)
	}
	if srv.recorder.Recording(l.TargetUDID) {
		t.Fatal("recording before boot")
	}
	// Boot outside the HTTP handler, as if an asynchronous boot just
	// completed: only the auto-record poller can start the recording.
	if err := reg.Boot(context.Background(), l.TargetUDID); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, 10*time.Second, func() bool {
		return srv.recorder.Recording(l.TargetUDID)
	})
	// Finalize the recording before TempDir cleanup races the still-
	// writing recorder child.
	srv.StopAllRecordings("daemon_shutdown")
}

func TestAcquireRejectsBadRecord(t *testing.T) {
	ts, _ := newRecordingTestServer(t)
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "rec-test", Record: "audio"}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func waitForCond(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
