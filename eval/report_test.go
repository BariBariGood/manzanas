package eval

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildReportDeterminismAndFlaky(t *testing.T) {
	sr := &ScenarioResult{
		Scenario: &Scenario{Name: "s1", Lease: LeaseSpec{Labels: []string{"simulator"}},
			Steps: []Step{{Name: "a", Op: "boot"}}},
		Runs: []RunResult{
			{Run: 1, OK: true, Steps: []StepResult{
				{Step: "a", Op: "boot", OK: true, LatencyMS: 100},
				{Step: "b", Op: "action", OK: true, LatencyMS: 10},
			}, SavedHashes: map[string]string{"root": "h1"}},
			{Run: 2, OK: false, Steps: []StepResult{
				{Step: "a", Op: "boot", OK: true, LatencyMS: 200},
				{Step: "b", Op: "action", OK: false, Error: "boom", LatencyMS: 30},
			}},
			{Run: 3, OK: true, Steps: []StepResult{
				{Step: "a", Op: "boot", OK: true, LatencyMS: 300},
				{Step: "b", Op: "action", OK: true, LatencyMS: 20},
			}, SavedHashes: map[string]string{"root": "h1"}},
		},
	}
	rep := BuildReport("http://example:7433", 3, []*ScenarioResult{sr})
	s := rep.Scenarios[0]
	if s.Passed != 2 || s.Runs != 3 {
		t.Fatalf("passed/runs = %d/%d", s.Passed, s.Runs)
	}
	if s.DeterminismRate < 0.66 || s.DeterminismRate > 0.67 {
		t.Errorf("determinism rate = %v", s.DeterminismRate)
	}
	if s.HashConsistent == nil || !*s.HashConsistent {
		t.Errorf("hash consistency = %v", s.HashConsistent)
	}
	if len(s.FlakySteps) != 1 || s.FlakySteps[0] != "b" {
		t.Errorf("flaky steps = %v", s.FlakySteps)
	}
	if len(s.Steps) != 2 || s.Steps[0].Step != "a" || s.Steps[1].Step != "b" {
		t.Fatalf("step summaries = %+v", s.Steps)
	}
	// p50 of [100,200,300] (nearest-rank) = 200, max = 300.
	if s.Steps[0].P50MS != 200 || s.Steps[0].MaxMS != 300 {
		t.Errorf("boot p50/max = %v/%v", s.Steps[0].P50MS, s.Steps[0].MaxMS)
	}
}

func TestHashConsistencyAcrossRunsDetectsDrift(t *testing.T) {
	runs := []RunResult{
		{OK: true, SavedHashes: map[string]string{"root": "h1"}},
		{OK: true, SavedHashes: map[string]string{"root": "h2"}},
	}
	c := hashConsistency(runs)
	if c == nil || *c {
		t.Errorf("expected inconsistent hashes, got %v", c)
	}
	if hashConsistency([]RunResult{{OK: true}}) != nil {
		t.Error("expected nil when no hashes saved")
	}
}

func TestReportRenders(t *testing.T) {
	sr := &ScenarioResult{
		Scenario: &Scenario{Name: "s1", Description: "desc",
			Lease: LeaseSpec{Labels: []string{"simulator"}},
			Steps: []Step{{Name: "a", Op: "boot"}}},
		Runs: []RunResult{{Run: 1, OK: true, TargetUDID: "U1",
			Steps: []StepResult{{Step: "a", Op: "boot", OK: true, LatencyMS: 5}}}},
	}
	rep := BuildReport("http://example:7433", 1, []*ScenarioResult{sr})
	md := rep.Markdown()
	for _, want := range []string{"# manzanasd eval report", "| s1 | 1 | 1 | 100% |", "## Scenario: s1", "run 1: **PASS**"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	raw, err := rep.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Scenarios[0].Name != "s1" {
		t.Errorf("JSON round-trip broke: %+v", back)
	}
}

func TestPercentile(t *testing.T) {
	vals := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if got := percentile(vals, 50); got != 50 {
		t.Errorf("p50 = %v", got)
	}
	if got := percentile(vals, 90); got != 90 {
		t.Errorf("p90 = %v", got)
	}
	if got := percentile(vals, 100); got != 100 {
		t.Errorf("p100 = %v", got)
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("empty = %v", got)
	}
}
