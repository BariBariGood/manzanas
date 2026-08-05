package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/internal/state"
	"github.com/BariBariGood/manzanas/proto"
)

// fakeEngine records the UDIDs the server hands to the state engine, so
// tests can assert the lease guardrails.
type fakeEngine struct {
	snapshotUDID string
	restoreUDID  string
	fixtureUDID  string
	fixtureName  string
	snaps        []proto.SnapshotInfo
	deletedID    string
	err          error
}

func (f *fakeEngine) Snapshot(ctx context.Context, udid, label string) (proto.SnapshotInfo, error) {
	f.snapshotUDID = udid
	return proto.SnapshotInfo{ID: "snp_test", SourceUDID: udid, Label: label}, f.err
}

func (f *fakeEngine) Restore(ctx context.Context, udid, snapshotID string, reboot bool) (bool, error) {
	f.restoreUDID = udid
	return reboot, f.err
}

func (f *fakeEngine) ListSnapshots(ctx context.Context) ([]proto.SnapshotInfo, error) {
	return f.snaps, f.err
}

func (f *fakeEngine) DeleteSnapshot(ctx context.Context, id string) error {
	f.deletedID = id
	return f.err
}
func (f *fakeEngine) Erase(ctx context.Context, udid string) error       { return f.err }
func (f *fakeEngine) Reset(ctx context.Context, udid, spec string) error { return f.err }
func (f *fakeEngine) ApplyFixture(ctx context.Context, udid, name string, payload map[string]any) error {
	f.fixtureUDID, f.fixtureName = udid, name
	return f.err
}

func newStateTestServer(t *testing.T, eng state.Engine) (*httptest.Server, *lease.Manager) {
	t.Helper()
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	srv.SetState(eng)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, leases
}

func acquireStateLease(t *testing.T, ts *httptest.Server, agent string) proto.Lease {
	t.Helper()
	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"simulator"}, AgentID: agent}, &l)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("acquire: %d", resp.StatusCode)
	}
	return l
}

func TestStateOpsAreJournaled(t *testing.T) {
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	store, err := journal.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv.SetJournal(journal.NewRecorder(store))
	eng := &fakeEngine{}
	srv.SetState(eng)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	l := acquireStateLease(t, ts, "state-agent")
	doJSON(t, "POST", ts.URL+"/v0/state/snapshots", proto.SnapshotRequest{LeaseID: l.ID, Label: "clean"}, nil)
	doJSON(t, "POST", ts.URL+"/v0/state/restore", proto.RestoreRequest{LeaseID: l.ID, Snapshot: "snp_test"}, nil)
	doJSON(t, "POST", ts.URL+"/v0/state/erase", proto.EraseRequest{LeaseID: l.ID}, nil)
	eng.err = state.ErrTargetBusy
	doJSON(t, "POST", ts.URL+"/v0/state/fixtures",
		proto.FixtureRequest{LeaseID: l.ID, Name: "statusbar"}, nil)

	entries, err := store.Read(context.Background(), l.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{} // action -> status
	for _, e := range entries {
		if e.Kind != "state" {
			continue
		}
		if e.Payload["agent_id"] != "state-agent" {
			t.Fatalf("state entry missing agent_id: %+v", e.Payload)
		}
		got[e.Payload["action"].(string)] = e.Payload["status"].(string)
	}
	want := map[string]string{
		proto.MethodStateSnapshot: "ok",
		proto.MethodStateRestore:  "ok",
		proto.MethodStateErase:    "ok",
		proto.MethodStateFixture:  "error", // engine returned target_busy
	}
	for action, status := range want {
		if got[action] != status {
			t.Fatalf("state entries = %v, want %s=%s", got, action, status)
		}
	}
}

func TestStateOpsRequireActiveLease(t *testing.T) {
	eng := &fakeEngine{}
	ts, _ := newStateTestServer(t, eng)

	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/state/snapshots", proto.SnapshotRequest{}, &e)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing lease_id: %d", resp.StatusCode)
	}
	resp = doJSON(t, "POST", ts.URL+"/v0/state/snapshots", proto.SnapshotRequest{LeaseID: "lse_nope"}, &e)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown lease: %d", resp.StatusCode)
	}
	if eng.snapshotUDID != "" {
		t.Fatal("engine reached without a valid lease")
	}
}

func TestStateOpsUseLeasedTargetOnly(t *testing.T) {
	eng := &fakeEngine{}
	ts, _ := newStateTestServer(t, eng)
	l := acquireStateLease(t, ts, "a1")

	var info proto.SnapshotInfo
	resp := doJSON(t, "POST", ts.URL+"/v0/state/snapshots",
		proto.SnapshotRequest{LeaseID: l.ID, Label: "clean"}, &info)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("snapshot: %d", resp.StatusCode)
	}
	// The server derives the UDID from the lease; a client can never point
	// the engine at a target it doesn't hold.
	if eng.snapshotUDID != l.TargetUDID {
		t.Fatalf("engine got %q, lease holds %q", eng.snapshotUDID, l.TargetUDID)
	}

	var ok map[string]any
	resp = doJSON(t, "POST", ts.URL+"/v0/state/fixtures",
		proto.FixtureRequest{LeaseID: l.ID, Name: "statusbar", Payload: map[string]any{"time": "9:41"}}, &ok)
	if resp.StatusCode != http.StatusOK || eng.fixtureUDID != l.TargetUDID || eng.fixtureName != "statusbar" {
		t.Fatalf("fixture: %d %q %q", resp.StatusCode, eng.fixtureUDID, eng.fixtureName)
	}
}

func TestStateReleasedLeaseRejected(t *testing.T) {
	eng := &fakeEngine{}
	ts, _ := newStateTestServer(t, eng)
	l := acquireStateLease(t, ts, "a1")
	doJSON(t, "DELETE", ts.URL+"/v0/leases/"+l.ID, nil, nil)

	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/state/restore",
		proto.RestoreRequest{LeaseID: l.ID, Snapshot: "snp_x"}, &e)
	if resp.StatusCode != http.StatusGone || e.Code != proto.ErrLeaseExpired {
		t.Fatalf("got %d %+v", resp.StatusCode, e)
	}
	if eng.restoreUDID != "" {
		t.Fatal("engine reached with a released lease")
	}
}

func TestStateTargetBusyMapping(t *testing.T) {
	eng := &fakeEngine{err: state.ErrTargetBusy}
	ts, _ := newStateTestServer(t, eng)
	l := acquireStateLease(t, ts, "a1")

	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/state/restore",
		proto.RestoreRequest{LeaseID: l.ID, Snapshot: "snp_x"}, &e)
	if resp.StatusCode != http.StatusConflict || e.Code != proto.ErrTargetBusy {
		t.Fatalf("got %d %+v", resp.StatusCode, e)
	}
}

func TestAcquireRejectsBadResetSpec(t *testing.T) {
	ts, _ := newStateTestServer(t, &fakeEngine{})
	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"simulator"}, AgentID: "a1", Reset: "bogus"}, &e)
	if resp.StatusCode != http.StatusBadRequest || e.Code != proto.ErrBadRequest {
		t.Fatalf("got %d %+v", resp.StatusCode, e)
	}
}

func TestSnapshotDeleteRequiresLeaseOnSource(t *testing.T) {
	eng := &fakeEngine{}
	ts, _ := newStateTestServer(t, eng)
	l := acquireStateLease(t, ts, "a1")
	eng.snaps = []proto.SnapshotInfo{
		{ID: "snp_mine", SourceUDID: l.TargetUDID},
		{ID: "snp_theirs", SourceUDID: "OTHER-UDID"},
	}

	// No lease: rejected before the engine is reached.
	var e proto.Error
	resp := doJSON(t, "DELETE", ts.URL+"/v0/state/snapshots/snp_mine", nil, &e)
	if resp.StatusCode != http.StatusBadRequest || eng.deletedID != "" {
		t.Fatalf("missing lease_id: %d %q", resp.StatusCode, eng.deletedID)
	}

	// Foreign snapshot: not found, engine never reached.
	resp = doJSON(t, "DELETE", ts.URL+"/v0/state/snapshots/snp_theirs?lease_id="+l.ID, nil, &e)
	if resp.StatusCode != http.StatusNotFound || eng.deletedID != "" {
		t.Fatalf("foreign snapshot: %d %q", resp.StatusCode, eng.deletedID)
	}

	// Own snapshot: allowed.
	var ok map[string]any
	resp = doJSON(t, "DELETE", ts.URL+"/v0/state/snapshots/snp_mine?lease_id="+l.ID, nil, &ok)
	if resp.StatusCode != http.StatusOK || eng.deletedID != "snp_mine" {
		t.Fatalf("own snapshot: %d %q", resp.StatusCode, eng.deletedID)
	}
}

func TestSnapshotListScopedToLeasedTarget(t *testing.T) {
	eng := &fakeEngine{}
	ts, _ := newStateTestServer(t, eng)
	l := acquireStateLease(t, ts, "a1")
	eng.snaps = []proto.SnapshotInfo{
		{ID: "snp_mine", SourceUDID: l.TargetUDID},
		{ID: "snp_theirs", SourceUDID: "OTHER-UDID"},
	}

	var e proto.Error
	resp := doJSON(t, "GET", ts.URL+"/v0/state/snapshots", nil, &e)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing lease_id: %d", resp.StatusCode)
	}

	var out struct {
		Snapshots []proto.SnapshotInfo `json:"snapshots"`
	}
	resp = doJSON(t, "GET", ts.URL+"/v0/state/snapshots?lease_id="+l.ID, nil, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	if len(out.Snapshots) != 1 || out.Snapshots[0].ID != "snp_mine" {
		t.Fatalf("foreign snapshots leaked: %+v", out.Snapshots)
	}
}

func TestResetSpecRejectedWithoutStateEngine(t *testing.T) {
	ts := newTestServer(t) // no state engine wired
	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"simulator"}, AgentID: "a1", Reset: "erase"}, &e)
	if resp.StatusCode != http.StatusNotImplemented || e.Code != proto.ErrNotImplemented {
		t.Fatalf("want not_implemented for reset without engine, got %d %+v", resp.StatusCode, e)
	}
}

func TestClearQuarantineFreesTarget(t *testing.T) {
	eng := &fakeEngine{err: context.DeadlineExceeded} // every reset fails
	ts, leases := newStateTestServer(t, eng)

	// Release a lease with a reset spec; the failed reset quarantines the
	// target.
	leases.SetResetFunc(func(pl proto.Lease) error {
		return eng.Reset(context.Background(), pl.TargetUDID, pl.Reset)
	})
	var withReset proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"simulator"}, AgentID: "a1", Reset: "erase"}, &withReset)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("acquire: %d", resp.StatusCode)
	}
	doJSON(t, "DELETE", ts.URL+"/v0/leases/"+withReset.ID, nil, nil)

	// Retry: clear-quarantine answers 409 target_busy until the failed
	// reset goroutine has unwound.
	var out map[string]any
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+withReset.TargetUDID+"/clear-quarantine", nil, &out)
		if resp.StatusCode == http.StatusOK {
			break
		}
		if resp.StatusCode != http.StatusConflict || time.Now().After(deadline) {
			t.Fatalf("clear-quarantine: %d", resp.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// degradeEngine fails snapshot resets with ErrSnapshotNotFound and records
// the specs it saw, to exercise the ResetSink erase fallback.
type degradeEngine struct {
	fakeEngine
	mu    sync.Mutex
	specs []string
}

func (d *degradeEngine) Reset(ctx context.Context, udid, spec string) error {
	d.mu.Lock()
	d.specs = append(d.specs, spec)
	d.mu.Unlock()
	if strings.HasPrefix(spec, state.ResetSnapshotPrefix) {
		return state.ErrSnapshotNotFound
	}
	return nil
}

func TestResetSinkDegradesMissingSnapshotToErase(t *testing.T) {
	eng := &degradeEngine{}
	reg := registry.NewMock(proto.Target{
		UDID: "MOCK-DEGRADE-0001", Kind: proto.TargetSimulator,
		Name: "iPhone 17 Pro", Runtime: "iOS 26.5",
		DeviceType: "iPhone 17 Pro", State: proto.StateShutdown,
	})
	srv := New(reg, nil, nil)
	srv.SetState(eng)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	leases.SetResetFunc(srv.ResetSink())

	l, err := leases.Acquire(context.Background(),
		proto.AcquireLeaseRequest{Labels: []string{"simulator"}, AgentID: "a1", Reset: "snapshot:gone"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := leases.Release(context.Background(), l.ID); err != nil {
		t.Fatalf("release: %v", err)
	}

	// The failed snapshot reset must degrade to an erase and free the
	// target rather than quarantining it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		eng.mu.Lock()
		specs := append([]string(nil), eng.specs...)
		eng.mu.Unlock()
		if len(specs) == 2 && specs[0] == "snapshot:gone" && specs[1] == state.ResetErase {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reset specs: %v", specs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		l2, err := leases.Acquire(context.Background(),
			proto.AcquireLeaseRequest{Labels: []string{"simulator"}, AgentID: "a2"})
		if err == nil && l2.State == proto.LeaseActive && l2.TargetUDID == l.TargetUDID {
			break
		}
		if err == nil {
			// Still quarantined: drop the queued lease and retry.
			_, _ = leases.Release(context.Background(), l2.ID)
		}
		if time.Now().After(deadline) {
			t.Fatal("target still quarantined after degraded reset")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUnknownStatePathReturnsJSONError(t *testing.T) {
	ts, _ := newStateTestServer(t, &fakeEngine{})
	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/state/bogus", nil, &e)
	if resp.StatusCode != http.StatusNotImplemented || e.Code != proto.ErrNotImplemented {
		t.Fatalf("want structured not_implemented, got %d %+v", resp.StatusCode, e)
	}
}

func TestStateNotImplementedWithoutEngine(t *testing.T) {
	ts := newTestServer(t)
	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/state/snapshots", proto.SnapshotRequest{}, &e)
	if resp.StatusCode != http.StatusNotImplemented || e.Code != proto.ErrNotImplemented {
		t.Fatalf("got %d %+v", resp.StatusCode, e)
	}
}
