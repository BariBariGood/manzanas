package eval

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

func pinScenario(udid string) *Scenario {
	return &Scenario{
		Name:  "test-pin",
		Lease: LeaseSpec{Labels: []string{"simulator"}, UDID: udid},
		Steps: []Step{{Name: "boot", Op: "boot"}},
	}
}

func TestScenarioUDIDPinPassedThrough(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()

	runner := NewRunner(NewClient(fd.URL()), RunnerConfig{Runs: 1})
	sr, err := runner.RunScenario(context.Background(), pinScenario("FAKE-UDID-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !sr.Runs[0].OK {
		t.Fatalf("run failed: %+v", sr.Runs[0])
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.acquireUDIDs) != 1 || fd.acquireUDIDs[0] != "FAKE-UDID-1" {
		t.Fatalf("acquire udids = %v", fd.acquireUDIDs)
	}
}

func TestRunnerTargetUDIDOverridesScenario(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()

	runner := NewRunner(NewClient(fd.URL()), RunnerConfig{Runs: 1, TargetUDID: "FAKE-UDID-1"})
	sr, err := runner.RunScenario(context.Background(), pinScenario("SOME-OTHER-UDID"))
	if err != nil {
		t.Fatal(err)
	}
	if !sr.Runs[0].OK {
		t.Fatalf("run failed: %+v", sr.Runs[0])
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if len(fd.acquireUDIDs) != 1 || fd.acquireUDIDs[0] != "FAKE-UDID-1" {
		t.Fatalf("acquire udids = %v", fd.acquireUDIDs)
	}
}

func TestUnknownUDIDPinFailsRun(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()

	runner := NewRunner(NewClient(fd.URL()), RunnerConfig{Runs: 1})
	sr, err := runner.RunScenario(context.Background(), pinScenario("NO-SUCH-UDID"))
	if err != nil {
		t.Fatal(err)
	}
	rr := sr.Runs[0]
	if rr.OK || !strings.Contains(rr.Error, "no_match") {
		t.Fatalf("expected no_match acquire failure, got %+v", rr)
	}
}

func TestScenarioValidateUDIDOnly(t *testing.T) {
	s := &Scenario{
		Name:  "udid-only",
		Lease: LeaseSpec{UDID: "SOME-UDID"},
		Steps: []Step{{Op: "boot"}},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("udid-only lease should validate: %v", err)
	}
	s.Lease.UDID = ""
	if err := s.Validate(); err == nil {
		t.Fatal("lease with no labels and no udid should not validate")
	}
}

func TestBootRetriesOverloaded(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()
	fd.overloadBoots = 2

	var slept []time.Duration
	client := NewClient(fd.URL())
	client.OverloadBudget = time.Minute
	client.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}

	runner := NewRunner(client, RunnerConfig{Runs: 1})
	sr, err := runner.RunScenario(context.Background(), passingScenario())
	if err != nil {
		t.Fatal(err)
	}
	if !sr.Runs[0].OK {
		t.Fatalf("run failed: %+v", sr.Runs[0])
	}
	if len(slept) != 2 {
		t.Fatalf("expected 2 backoff sleeps, got %v", slept)
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if fd.bootAttempts != 3 {
		t.Fatalf("expected 3 boot attempts, got %d", fd.bootAttempts)
	}
}

func TestBootOverloadedBudgetExhausted(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()
	fd.overloadBoots = 1000

	client := NewClient(fd.URL())
	client.OverloadBudget = 10 * time.Millisecond
	client.sleep = func(ctx context.Context, d time.Duration) error { return nil }

	runner := NewRunner(client, RunnerConfig{Runs: 1})
	sr, err := runner.RunScenario(context.Background(), passingScenario())
	if err != nil {
		t.Fatal(err)
	}
	rr := sr.Runs[0]
	if rr.OK {
		t.Fatal("run should fail once the overload budget is exhausted")
	}
	if len(rr.Steps) != 1 || !strings.Contains(rr.Steps[0].Error, "overload retry budget") {
		t.Fatalf("expected budget-exhausted boot step error, got %+v", rr.Steps)
	}
}

func TestBootOverloadedNoBudgetFailsFast(t *testing.T) {
	fd := newFakeDaemon()
	defer fd.Close()
	fd.overloadBoots = 1

	client := NewClient(fd.URL()) // OverloadBudget = 0
	err := func() error {
		lease, err := client.AcquireLease(context.Background(), proto.AcquireLeaseRequest{
			Labels: []string{"simulator"}, AgentID: "test",
		})
		if err != nil {
			t.Fatal(err)
		}
		defer client.ReleaseLease(context.Background(), lease.ID)
		return client.Boot(context.Background(), lease.TargetUDID, lease.ID)
	}()
	if !IsOverloaded(err) {
		t.Fatalf("expected the raw overloaded error, got %v", err)
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if fd.bootAttempts != 1 {
		t.Fatalf("expected exactly 1 boot attempt, got %d", fd.bootAttempts)
	}
}

func TestProfileScalesTimeouts(t *testing.T) {
	p, err := ProfileByName("intel")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.scaleTimeout(10 * time.Second); got != 30*time.Second {
		t.Fatalf("intel scaleTimeout(10s) = %s", got)
	}
	var zero TimingProfile
	if got := zero.scaleTimeout(10 * time.Second); got != 10*time.Second {
		t.Fatalf("zero profile scaleTimeout(10s) = %s", got)
	}
	// A partially-filled profile treats the unset multiplier as identity.
	partial := TimingProfile{WaitScale: 2}
	if got := partial.scaleTimeout(10 * time.Second); got != 10*time.Second {
		t.Fatalf("partial profile scaleTimeout(10s) = %s", got)
	}
	if _, err := ProfileByName("nope"); err == nil {
		t.Fatal("unknown profile should error")
	}
}

func TestProfileScalesWaitPayloads(t *testing.T) {
	p := TimingProfile{Name: "intel", TimeoutScale: 3, WaitScale: 2}
	in := map[string]any{"timeout_ms": 30000, "interval_ms": 1000, "stable_samples": 5}
	out := p.scaleWaitPayload("wait_tree_stable", in)
	if out["timeout_ms"] != float64(60000) || out["interval_ms"] != float64(2000) {
		t.Fatalf("scaled payload = %v", out)
	}
	if out["stable_samples"] != 5 {
		t.Fatalf("stable_samples must not be scaled: %v", out)
	}
	if in["timeout_ms"] != 30000 {
		t.Fatalf("input payload was mutated: %v", in)
	}
	// Scaling never pushes timeout_ms past the daemon's 2m clamp; the whole
	// payload shares the reduced factor so the poll count is preserved.
	capped := p.scaleWaitPayload("wait_tree_stable", map[string]any{"timeout_ms": 90000, "interval_ms": 1000})
	if capped["timeout_ms"] != float64(120000) || capped["interval_ms"] != float64(1333) {
		t.Fatalf("capped payload = %v", capped)
	}
	// At the clamp already: nothing to gain, payload passes through unchanged.
	atCap := map[string]any{"timeout_ms": 120000, "interval_ms": 1000}
	if got := p.scaleWaitPayload("wait_tree_stable", atCap); got["interval_ms"] != 1000 {
		t.Fatalf("at-cap payload changed: %v", got)
	}
	// Missing timeout_ms: the daemon's per-kind default budget is
	// materialized and scaled so it grows with interval_ms.
	noTimeout := p.scaleWaitPayload("wait_tree_stable", map[string]any{"interval_ms": 1000})
	if noTimeout["timeout_ms"] != float64(30000) || noTimeout["interval_ms"] != float64(2000) {
		t.Fatalf("wait_tree_stable payload without timeout_ms = %v", noTimeout)
	}
	noTimeoutEl := p.scaleWaitPayload("wait_for_element", map[string]any{"interval_ms": 1000})
	if noTimeoutEl["timeout_ms"] != float64(20000) {
		t.Fatalf("wait_for_element payload without timeout_ms = %v", noTimeoutEl)
	}
	// A bare wait step (nil payload) still gets its default budget scaled.
	bare := p.scaleWaitPayload("wait_tree_stable", nil)
	if bare["timeout_ms"] != float64(30000) {
		t.Fatalf("bare wait_tree_stable payload = %v", bare)
	}
	// A partially-filled profile (WaitScale unset) leaves payloads alone.
	partial := TimingProfile{TimeoutScale: 3}
	if got := partial.scaleWaitPayload("wait_tree_stable", map[string]any{"interval_ms": 1000}); got["interval_ms"] != 1000 {
		t.Fatalf("partial profile changed payload: %v", got)
	}
	// Non-wait kinds pass through untouched.
	tap := map[string]any{"x": 10, "y": 20}
	if got := p.scaleWaitPayload("tap", tap); got["x"] != 10 || got["y"] != 20 {
		t.Fatalf("tap payload changed: %v", got)
	}
}
