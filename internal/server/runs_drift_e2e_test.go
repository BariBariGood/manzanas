package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BariBariGood/manzanas/internal/actions/mockapp"
	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/internal/runspec"
	"github.com/BariBariGood/manzanas/proto"
)

// driftTestServer is newRunTestServer plus handles on the journal store
// and the mock app store, so tests can export recorded runs and mutate
// app state between record and replay.
func driftTestServer(t *testing.T) (*httptest.Server, journal.Store, *mockapp.Store) {
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
	return ts, js, store
}

// recordSpec is the login flow recorded with post-step expectations.
func recordSpec() proto.RunSpec {
	return proto.RunSpec{
		Name:               "drift-demo",
		Target:             proto.RunTarget{Labels: []string{"ios26"}},
		RecordExpectations: true,
		Steps: []proto.RunStep{
			{Action: "tap_element", With: map[string]any{"id": "username"}},
			{Action: "type_into_element", With: map[string]any{"id": "username", "text": "agent"}},
			{Action: "type_into_element", With: map[string]any{"id": "password", "text": "pw"}},
			{Action: "tap_element", With: map[string]any{"label": "Sign In"}},
			{Action: "wait_for_element", With: map[string]any{"label": "Welcome, agent!", "timeout_ms": 5000}},
		},
		Timeouts: proto.RunTimeouts{RunSeconds: 60},
	}
}

// exportWithExpectations reads the recorded run's journal and exports the
// replay spec with expect blocks, round-tripping through the strict
// parser exactly like `manzanas journal to-spec --with-expectations`.
func exportWithExpectations(t *testing.T, js journal.Store, runID string) proto.RunSpec {
	t.Helper()
	meta, err := js.ReadMeta(runID)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	entries, err := js.Read(context.Background(), runID, 0, 10000)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	out, err := journal.ExportSpecYAML(meta, entries, journal.SpecOptions{WithExpectations: true})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	spec, err := runspec.Parse(out)
	if err != nil {
		t.Fatalf("re-parse exported spec: %v\n%s", err, out)
	}
	return spec
}

func runSync(t *testing.T, ts *httptest.Server, spec proto.RunSpec) proto.Run {
	t.Helper()
	var run proto.Run
	resp := doJSON(t, "POST", ts.URL+"/v0/runs", proto.RunRequest{AgentID: "drift-test", Spec: spec}, &run)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v0/runs: %d %+v", resp.StatusCode, run)
	}
	return run
}

// TestRunDriftRecordReplay drives the whole record → export → replay
// loop: a recorded run's expectations replay clean (trusted), and after
// the mock app's state is mutated the same replay reports drift and the
// verdict degrades — or fails under strict_drift.
func TestRunDriftRecordReplay(t *testing.T) {
	ts, js, _ := driftTestServer(t)

	// Record with post-step expectations journaled.
	rec := runSync(t, ts, recordSpec())
	if rec.State != proto.RunPassed {
		t.Fatalf("record run: %s (%+v)", rec.State, rec.Error)
	}
	if rec.Verdict != proto.VerdictUnverified {
		t.Fatalf("record verdict = %s, want unverified (no expect blocks yet)", rec.Verdict)
	}

	if meta, err := js.ReadMeta(rec.LeaseID); err != nil || meta.Verdict != string(proto.VerdictUnverified) {
		t.Fatalf("journal meta verdict = %q (err %v), want unverified", meta.Verdict, err)
	}

	replay := exportWithExpectations(t, js, rec.LeaseID)
	expects := 0
	for _, st := range replay.Steps {
		if st.Expect != nil {
			expects++
			if len(st.Expect.Texts) == 0 || st.Expect.Fingerprint == "" {
				t.Fatalf("expect block incomplete: %+v", st.Expect)
			}
		}
	}
	if expects == 0 {
		t.Fatalf("no expect blocks exported: %+v", replay.Steps)
	}

	// Replay unchanged on a fresh daemon: no drift, all expects pass.
	ts2, _, _ := driftTestServer(t)
	run2 := runSync(t, ts2, replay)
	if run2.State != proto.RunPassed {
		t.Fatalf("clean replay: %s (%+v)", run2.State, run2.Error)
	}
	if run2.Verdict != proto.VerdictTrusted {
		t.Fatalf("clean replay verdict = %s, want trusted (steps %+v)", run2.Verdict, run2.Steps)
	}
	for _, st := range run2.Steps {
		if st.Drifted {
			t.Fatalf("clean replay drifted: %+v", st)
		}
	}

	// Mutate the app (scroll the content) before replaying: the screen
	// no longer matches the recorded fingerprints => drift, verdict
	// capped at degraded, run still passes.
	ts3, _, store3 := driftTestServer(t)
	replay.Target = proto.RunTarget{UDID: rec.TargetUDID}
	store3.App(rec.TargetUDID).Swipe(0, 300, 0, 200)
	run3 := runSync(t, ts3, replay)
	if run3.State != proto.RunPassed {
		t.Fatalf("drifted replay: %s (%+v)", run3.State, run3.Error)
	}
	if run3.Verdict != proto.VerdictDegraded {
		t.Fatalf("drifted replay verdict = %s, want degraded (steps %+v)", run3.Verdict, run3.Steps)
	}
	drifted := 0
	for _, st := range run3.Steps {
		if st.Drifted {
			drifted++
			if st.DriftDetail == "" {
				t.Fatalf("drifted step without detail: %+v", st)
			}
		}
	}
	if drifted == 0 {
		t.Fatalf("no step drifted: %+v", run3.Steps)
	}

	// Same mutation under strict_drift: the run fails at the first
	// drifted step with assertion_failed.
	ts4, _, store4 := driftTestServer(t)
	strict := replay
	strict.StrictDrift = true
	store4.App(rec.TargetUDID).Swipe(0, 300, 0, 200)
	run4 := runSync(t, ts4, strict)
	if run4.State != proto.RunFailed {
		t.Fatalf("strict drift run: %s, want failed", run4.State)
	}
	if run4.Error == nil || run4.Error.Code != proto.ErrAssertionFailed {
		t.Fatalf("strict drift error = %+v, want assertion_failed", run4.Error)
	}
}

// TestRunVerifiedTapLiftsIntoStepResult checks the verify:true report is
// mirrored onto the step result and graded into the verdict.
func TestRunVerifiedTapLiftsIntoStepResult(t *testing.T) {
	ts, _, _ := driftTestServer(t)
	run := runSync(t, ts, proto.RunSpec{
		Target: proto.RunTarget{Labels: []string{"ios26"}},
		Steps: []proto.RunStep{
			// Tapping the Wi-Fi switch visibly changes the screen.
			{Action: "tap", With: map[string]any{"x": 50, "y": 322, "verify": true, "verify_settle_ms": 10}},
		},
		Timeouts: proto.RunTimeouts{RunSeconds: 60},
	})
	if run.State != proto.RunPassed {
		t.Fatalf("run: %s (%+v)", run.State, run.Error)
	}
	if len(run.Steps) != 1 || run.Steps[0].Verified == nil || !*run.Steps[0].Verified {
		t.Fatalf("steps = %+v, want verified:true", run.Steps)
	}
	if run.Verdict != proto.VerdictTrusted {
		t.Fatalf("verdict = %s, want trusted", run.Verdict)
	}
}
