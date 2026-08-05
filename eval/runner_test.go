package eval

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func passingScenario() *Scenario {
	return &Scenario{
		Name:  "test-pass",
		Lease: LeaseSpec{Labels: []string{"simulator"}},
		Steps: []Step{
			{Name: "boot", Op: "boot"},
			{Name: "launch", Op: "action", Kind: "launch_app",
				Payload: map[string]any{"bundle_id": "com.apple.Preferences"}},
			{Name: "assert-general", Op: "assert",
				Assert: &Assertion{ElementExists: &ElementQuery{Label: "General"}}},
			{Name: "save-hash", Op: "assert",
				Assert: &Assertion{TreeHash: &TreeHashAssertion{SaveAs: "root"}}},
			{Name: "check-hash", Op: "assert",
				Assert: &Assertion{TreeHash: &TreeHashAssertion{EqualsSaved: "root"}}},
			{Name: "shot", Op: "assert",
				Assert: &Assertion{Screenshot: &ScreenshotAssertion{Save: "shot"}}},
		},
	}
}

func TestRunnerAllRunsPass(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()

	dir := t.TempDir()
	runner := NewRunner(NewClient(fd.URL()), RunnerConfig{Runs: 3, ArtifactDir: dir})
	sr, err := runner.RunScenario(context.Background(), passingScenario())
	if err != nil {
		t.Fatal(err)
	}
	if len(sr.Runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(sr.Runs))
	}
	for _, rr := range sr.Runs {
		if !rr.OK {
			t.Errorf("run %d failed: %+v", rr.Run, rr)
		}
		if len(rr.Steps) != 6 {
			t.Errorf("run %d: expected 6 steps, got %d", rr.Run, len(rr.Steps))
		}
		if rr.SavedHashes["root"] != "hash-1" {
			t.Errorf("run %d: saved hash = %q", rr.Run, rr.SavedHashes["root"])
		}
		if len(rr.Artifacts) != 1 {
			t.Errorf("run %d: expected 1 artifact, got %v", rr.Run, rr.Artifacts)
		} else if filepath.Dir(rr.Artifacts[0]) != dir {
			t.Errorf("artifact outside dir: %s", rr.Artifacts[0])
		}
	}
	// Every run must release its lease.
	if got := len(fd.releasedLeases()); got != 3 {
		t.Errorf("expected 3 released leases, got %d", got)
	}
}

func TestRunnerStepFailureStopsRun(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()
	fd.failActionKinds["swipe"] = true

	s := &Scenario{
		Name:  "test-fail",
		Lease: LeaseSpec{Labels: []string{"simulator"}},
		Steps: []Step{
			{Name: "boot", Op: "boot"},
			{Name: "bad-swipe", Op: "action", Kind: "swipe"},
			{Name: "never-runs", Op: "wait", Duration: Duration(time.Millisecond)},
		},
	}
	runner := NewRunner(NewClient(fd.URL()), RunnerConfig{Runs: 2})
	sr, err := runner.RunScenario(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	for _, rr := range sr.Runs {
		if rr.OK {
			t.Errorf("run %d unexpectedly passed", rr.Run)
		}
		if len(rr.Steps) != 2 {
			t.Errorf("run %d: expected run to stop after failing step, got %d steps", rr.Run, len(rr.Steps))
		}
		last := rr.Steps[len(rr.Steps)-1]
		if last.OK || !strings.Contains(last.Error, "swipe failed") {
			t.Errorf("run %d: unexpected last step %+v", rr.Run, last)
		}
	}
	// Leases must be released even on failure.
	if got := len(fd.releasedLeases()); got != 2 {
		t.Errorf("expected 2 released leases, got %d", got)
	}
}

func TestRunnerNoMatchingTarget(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()

	s := &Scenario{
		Name:  "test-nomatch",
		Lease: LeaseSpec{Labels: []string{"no-such-label"}},
		Steps: []Step{{Name: "boot", Op: "boot"}},
	}
	runner := NewRunner(NewClient(fd.URL()), RunnerConfig{Runs: 1})
	sr, err := runner.RunScenario(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	rr := sr.Runs[0]
	if rr.OK || !strings.Contains(rr.Error, "no_match") {
		t.Errorf("expected no_match run error, got %+v", rr)
	}
}

func TestRunnerElementAbsentAndTreeHashMismatch(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()

	s := &Scenario{
		Name:  "test-absent",
		Lease: LeaseSpec{Labels: []string{"simulator"}},
		Steps: []Step{
			{Name: "boot", Op: "boot"},
			{Name: "absent-ok", Op: "assert",
				Assert: &Assertion{ElementAbsent: &ElementQuery{Label: "Nonexistent"}}},
			{Name: "literal-hash", Op: "assert", Timeout: Duration(time.Second),
				Assert: &Assertion{TreeHash: &TreeHashAssertion{Equals: "wrong-hash"}}},
		},
	}
	runner := NewRunner(NewClient(fd.URL()), RunnerConfig{Runs: 1})
	sr, err := runner.RunScenario(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	rr := sr.Runs[0]
	if rr.OK {
		t.Fatal("run unexpectedly passed")
	}
	if !rr.Steps[1].OK {
		t.Errorf("element_absent step failed: %s", rr.Steps[1].Error)
	}
	if rr.Steps[2].OK || !strings.Contains(rr.Steps[2].Error, "mismatch") {
		t.Errorf("expected hash mismatch, got %+v", rr.Steps[2])
	}
}

func TestRunnerSnapshotRestoreFlow(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()

	s := &Scenario{
		Name:  "test-snapshot",
		Lease: LeaseSpec{Labels: []string{"simulator"}, Reset: "erase"},
		Steps: []Step{
			{Name: "snap", Op: "snapshot", SnapshotLabel: "base"},
			{Name: "boot", Op: "boot"},
			{Name: "restore", Op: "restore", Snapshot: "base", Reboot: true},
			{Name: "shutdown", Op: "shutdown"},
			{Name: "erase", Op: "erase"},
		},
	}
	runner := NewRunner(NewClient(fd.URL()), RunnerConfig{Runs: 1})
	sr, err := runner.RunScenario(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !sr.Runs[0].OK {
		t.Fatalf("run failed: %+v", sr.Runs[0])
	}
	if !strings.Contains(sr.Runs[0].Steps[2].Detail, "rebooted=true") {
		t.Errorf("expected rebooted restore, got %q", sr.Runs[0].Steps[2].Detail)
	}
	if n := fd.snapshotCount(); n != 0 {
		t.Errorf("expected run teardown to delete its snapshots, %d left", n)
	}
}

func TestRunnerWaitStep(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()

	s := &Scenario{
		Name:  "test-wait",
		Lease: LeaseSpec{Labels: []string{"simulator"}},
		Steps: []Step{
			{Name: "settle", Op: "wait", Duration: Duration(50 * time.Millisecond)},
		},
	}
	runner := NewRunner(NewClient(fd.URL()), RunnerConfig{Runs: 1})
	sr, err := runner.RunScenario(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	rr := sr.Runs[0]
	if !rr.OK {
		t.Fatalf("run failed: %+v", rr)
	}
	if rr.Steps[0].Latency < 50*time.Millisecond {
		t.Errorf("wait step returned after %v, want >= 50ms", rr.Steps[0].Latency)
	}
}

func TestRunnerTerminalAssertionFailsFast(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()

	s := &Scenario{
		Name:  "test-terminal",
		Lease: LeaseSpec{Labels: []string{"simulator"}},
		Steps: []Step{
			{Name: "boot", Op: "boot"},
			// equals_saved names a hash that was never saved: this can
			// never become true, so it must fail without polling out the
			// generous step timeout.
			{Name: "missing-saved", Op: "assert", Timeout: Duration(30 * time.Second),
				Assert: &Assertion{TreeHash: &TreeHashAssertion{EqualsSaved: "never-saved"}}},
		},
	}
	runner := NewRunner(NewClient(fd.URL()), RunnerConfig{Runs: 1})
	start := time.Now()
	sr, err := runner.RunScenario(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	rr := sr.Runs[0]
	if rr.OK {
		t.Fatal("run unexpectedly passed")
	}
	last := rr.Steps[len(rr.Steps)-1]
	if last.OK || !strings.Contains(last.Error, "no saved tree hash") {
		t.Errorf("unexpected step result: %+v", last)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("terminal assertion took %v, expected fast failure", elapsed)
	}
}

func TestRunnerAssertionRetriesTransientFailure(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()
	// The first two observes fail; the assertion's poll loop must retry
	// and eventually pass within the step timeout.
	fd.failActionsOnce["observe"] = 2

	s := &Scenario{
		Name:  "test-retry",
		Lease: LeaseSpec{Labels: []string{"simulator"}},
		Steps: []Step{
			{Name: "boot", Op: "boot"},
			{Name: "eventually", Op: "assert", Timeout: Duration(30 * time.Second),
				Assert: &Assertion{ElementExists: &ElementQuery{Label: "General"}}},
		},
	}
	runner := NewRunner(NewClient(fd.URL()), RunnerConfig{Runs: 1})
	sr, err := runner.RunScenario(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !sr.Runs[0].OK {
		t.Fatalf("run failed: %+v", sr.Runs[0])
	}
}

func TestRunnerKeepaliveRenewsLease(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()

	s := &Scenario{
		Name: "test-keepalive",
		// 1s TTL -> keepalive interval ~333ms; the 1s wait spans it.
		Lease: LeaseSpec{Labels: []string{"simulator"}, TTLSeconds: 1},
		Steps: []Step{
			{Name: "settle", Op: "wait", Duration: Duration(time.Second)},
		},
	}
	runner := NewRunner(NewClient(fd.URL()), RunnerConfig{Runs: 1})
	sr, err := runner.RunScenario(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !sr.Runs[0].OK {
		t.Fatalf("run failed: %+v", sr.Runs[0])
	}
	fd.mu.Lock()
	renews := fd.renewCount
	fd.mu.Unlock()
	if renews == 0 {
		t.Error("expected at least one lease renewal during the run")
	}
}

func TestRunnerFixture(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()

	s := &Scenario{
		Name:  "test-fixture",
		Lease: LeaseSpec{Labels: []string{"simulator"}},
		Steps: []Step{
			{Name: "boot", Op: "boot"},
			{Name: "statusbar", Op: "fixture", Fixture: "statusbar",
				FixturePayload: map[string]any{"time": "9:41"}},
		},
	}
	runner := NewRunner(NewClient(fd.URL()), RunnerConfig{Runs: 1})
	sr, err := runner.RunScenario(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !sr.Runs[0].OK {
		t.Fatalf("run failed: %+v", sr.Runs[0])
	}
	if len(fd.fixtureLog) != 1 || fd.fixtureLog[0] != "statusbar" {
		t.Errorf("fixture log = %v", fd.fixtureLog)
	}
}
