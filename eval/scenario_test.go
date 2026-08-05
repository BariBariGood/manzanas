package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadScenarioYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.yaml")
	content := `
name: demo
lease:
  labels: [simulator]
  reset: erase
default_timeout: 90s
steps:
  - op: boot
    timeout: 300s
  - op: action
    kind: tap
    payload: {x: 100, y: 200}
  - op: wait
    duration: 2s
  - op: assert
    assert:
      element_exists: {label: General}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "demo" || s.Lease.Reset != "erase" {
		t.Errorf("scenario = %+v", s)
	}
	if s.DefaultTimeout.Std() != 90*time.Second {
		t.Errorf("default_timeout = %v", s.DefaultTimeout.Std())
	}
	if s.Steps[0].Timeout.Std() != 300*time.Second {
		t.Errorf("boot timeout = %v", s.Steps[0].Timeout.Std())
	}
	if x, ok := s.Steps[1].Payload["x"].(int); !ok || x != 100 {
		t.Errorf("tap payload = %#v", s.Steps[1].Payload)
	}
	if s.Steps[3].Assert.ElementExists.Label != "General" {
		t.Errorf("assert = %+v", s.Steps[3].Assert)
	}
}

func TestLoadScenarioJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	content := `{"name":"j","lease":{"labels":["simulator"]},"steps":[{"op":"boot"}]}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadScenario(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "j" {
		t.Errorf("scenario = %+v", s)
	}
}

func TestLoadScenarioRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "typo.yaml")
	content := `
name: typo-field
lease:
  labels: [simulator]
defualt_timeout: 5s
steps:
  - op: boot
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadScenario(path)
	if err == nil || !strings.Contains(err.Error(), "defualt_timeout") {
		t.Errorf("LoadScenario() = %v, want unknown-field error naming defualt_timeout", err)
	}
}

func TestScenarioValidation(t *testing.T) {
	cases := []struct {
		name    string
		s       Scenario
		wantErr string
	}{
		{"no name", Scenario{Lease: LeaseSpec{Labels: []string{"x"}}, Steps: []Step{{Op: "boot"}}}, "no name"},
		{"no labels", Scenario{Name: "s", Steps: []Step{{Op: "boot"}}}, "labels or a udid"},
		{"no steps", Scenario{Name: "s", Lease: LeaseSpec{Labels: []string{"x"}}}, "at least one step"},
		{"bad op", Scenario{Name: "s", Lease: LeaseSpec{Labels: []string{"x"}}, Steps: []Step{{Op: "frobnicate"}}}, "unknown op"},
		{"action no kind", Scenario{Name: "s", Lease: LeaseSpec{Labels: []string{"x"}}, Steps: []Step{{Op: "action"}}}, "needs kind"},
		{"fixture no name", Scenario{Name: "s", Lease: LeaseSpec{Labels: []string{"x"}}, Steps: []Step{{Op: "fixture"}}}, "needs fixture"},
		{"snapshot no label", Scenario{Name: "s", Lease: LeaseSpec{Labels: []string{"x"}}, Steps: []Step{{Op: "snapshot"}}}, "snapshot_label"},
		{"restore no snapshot", Scenario{Name: "s", Lease: LeaseSpec{Labels: []string{"x"}}, Steps: []Step{{Op: "restore"}}}, "needs snapshot"},
		{"wait no duration", Scenario{Name: "s", Lease: LeaseSpec{Labels: []string{"x"}}, Steps: []Step{{Op: "wait"}}}, "positive duration"},
		{"assert empty", Scenario{Name: "s", Lease: LeaseSpec{Labels: []string{"x"}}, Steps: []Step{{Op: "assert", Assert: &Assertion{}}}}, "exactly one assertion"},
		{"assert double", Scenario{Name: "s", Lease: LeaseSpec{Labels: []string{"x"}},
			Steps: []Step{{Op: "assert", Assert: &Assertion{
				ElementExists: &ElementQuery{Label: "a"},
				TreeHash:      &TreeHashAssertion{SaveAs: "x"},
			}}}}, "exactly one assertion"},
		{"tree hash empty", Scenario{Name: "s", Lease: LeaseSpec{Labels: []string{"x"}},
			Steps: []Step{{Op: "assert", Assert: &Assertion{TreeHash: &TreeHashAssertion{}}}}}, "tree_hash assertion"},
		{"wait timeout below duration", Scenario{Name: "s", Lease: LeaseSpec{Labels: []string{"x"}},
			Steps: []Step{{Op: "wait", Duration: Duration(8 * time.Second), Timeout: Duration(time.Second)}}}, "must exceed its duration"},
		{"wait timeout equals duration", Scenario{Name: "s", Lease: LeaseSpec{Labels: []string{"x"}},
			Steps: []Step{{Op: "wait", Duration: Duration(time.Second), Timeout: Duration(time.Second)}}}, "must exceed its duration"},
		{"duplicate step names", Scenario{Name: "s", Lease: LeaseSpec{Labels: []string{"x"}},
			Steps: []Step{{Name: "same", Op: "boot"}, {Name: "same", Op: "wait", Duration: Duration(time.Second)}}}, "same name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestShippedScenariosAreValid(t *testing.T) {
	matches, err := filepath.Glob("scenarios/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) < 3 {
		t.Fatalf("expected >=3 shipped scenarios, got %v", matches)
	}
	for _, path := range matches {
		if _, err := LoadScenario(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}
