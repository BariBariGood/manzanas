// Package eval is a reproducible agent-QA benchmark harness for manzanasd.
//
// A scenario file (YAML or JSON) describes a sequence of protocol ops —
// lease, fixtures, actions, assertions, release — and the runner executes
// it N times against a daemon (fresh lease + reset each run), recording
// per-run pass/fail and per-step latency, then emits a markdown + JSON
// report with determinism rate, flaky steps, and latency percentiles.
package eval

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from strings like "30s".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the standard-library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Scenario is one benchmark scenario: a lease spec plus an ordered list of
// steps executed under that lease.
type Scenario struct {
	Name        string    `yaml:"name" json:"name"`
	Description string    `yaml:"description,omitempty" json:"description,omitempty"`
	Lease       LeaseSpec `yaml:"lease" json:"lease"`
	// DefaultTimeout applies to steps that don't set their own (default 60s).
	DefaultTimeout Duration `yaml:"default_timeout,omitempty" json:"default_timeout,omitempty"`
	Steps          []Step   `yaml:"steps" json:"steps"`
}

// LeaseSpec is the lease acquired at the start of every run.
type LeaseSpec struct {
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	// UDID pins every run's lease to that specific target (which must also
	// match Labels, if any are given). The runner's TargetUDID overrides it.
	UDID       string `yaml:"udid,omitempty" json:"udid,omitempty"`
	Reset      string `yaml:"reset,omitempty" json:"reset,omitempty"` // "none" | "erase" | "snapshot:<name>"
	TTLSeconds int    `yaml:"ttl_seconds,omitempty" json:"ttl_seconds,omitempty"`
	// AcquireTimeout bounds waiting for a queued lease to become active
	// (default 5m).
	AcquireTimeout Duration `yaml:"acquire_timeout,omitempty" json:"acquire_timeout,omitempty"`
}

// Step is one operation in a scenario. Op selects which of the op-specific
// fields apply.
type Step struct {
	Name    string   `yaml:"name,omitempty" json:"name,omitempty"`
	Op      string   `yaml:"op" json:"op"` // boot|shutdown|action|fixture|snapshot|restore|erase|wait|assert
	Timeout Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`

	// op: action — dispatch an ActionRequest (tap, swipe, type, observe,
	// screenshot, launch_app, ...).
	Kind    string         `yaml:"kind,omitempty" json:"kind,omitempty"`
	Payload map[string]any `yaml:"payload,omitempty" json:"payload,omitempty"`

	// op: fixture — apply a named state fixture.
	Fixture        string         `yaml:"fixture,omitempty" json:"fixture,omitempty"`
	FixturePayload map[string]any `yaml:"fixture_payload,omitempty" json:"fixture_payload,omitempty"`

	// op: snapshot — capture a snapshot with this label.
	SnapshotLabel string `yaml:"snapshot_label,omitempty" json:"snapshot_label,omitempty"`

	// op: restore — restore a snapshot by ID or label; Reboot performs the
	// shutdown+restore+boot cycle on a booted target.
	Snapshot string `yaml:"snapshot,omitempty" json:"snapshot,omitempty"`
	Reboot   bool   `yaml:"reboot,omitempty" json:"reboot,omitempty"`

	// op: wait — sleep for a fixed duration.
	Duration Duration `yaml:"duration,omitempty" json:"duration,omitempty"`

	// op: assert — check a condition against the live target.
	Assert *Assertion `yaml:"assert,omitempty" json:"assert,omitempty"`
}

// Assertion is a checkable condition. Exactly one of the fields must be set.
type Assertion struct {
	// ElementExists / ElementAbsent run an observe and search the
	// accessibility tree for a matching element.
	ElementExists *ElementQuery `yaml:"element_exists,omitempty" json:"element_exists,omitempty"`
	ElementAbsent *ElementQuery `yaml:"element_absent,omitempty" json:"element_absent,omitempty"`
	// TreeHash runs an observe and records or compares the tree hash.
	TreeHash *TreeHashAssertion `yaml:"tree_hash,omitempty" json:"tree_hash,omitempty"`
	// Screenshot dispatches a screenshot action and verifies bytes were
	// captured; Save writes the PNG artifact under the report directory.
	Screenshot *ScreenshotAssertion `yaml:"screenshot,omitempty" json:"screenshot,omitempty"`
}

// ElementQuery matches accessibility-tree nodes. All set fields must match;
// Label and Value match as substrings, Role matches exactly.
type ElementQuery struct {
	Role  string `yaml:"role,omitempty" json:"role,omitempty"`
	Label string `yaml:"label,omitempty" json:"label,omitempty"`
	Value string `yaml:"value,omitempty" json:"value,omitempty"`
}

// empty reports whether no search criteria are set (such a query would
// match every node).
func (q *ElementQuery) empty() bool {
	return q.Role == "" && q.Label == "" && q.Value == ""
}

func (q *ElementQuery) String() string {
	var parts []string
	if q.Role != "" {
		parts = append(parts, "role="+q.Role)
	}
	if q.Label != "" {
		parts = append(parts, "label~"+q.Label)
	}
	if q.Value != "" {
		parts = append(parts, "value~"+q.Value)
	}
	return strings.Join(parts, " ")
}

// TreeHashAssertion records or checks the observe tree hash.
//   - SaveAs: store the hash under a name for later comparison in this run.
//     The runner also compares saved hashes ACROSS runs to compute the
//     determinism rate.
//   - EqualsSaved: the hash must equal one saved earlier in the same run.
//   - Equals: the hash must equal this literal value.
type TreeHashAssertion struct {
	SaveAs      string `yaml:"save_as,omitempty" json:"save_as,omitempty"`
	EqualsSaved string `yaml:"equals_saved,omitempty" json:"equals_saved,omitempty"`
	Equals      string `yaml:"equals,omitempty" json:"equals,omitempty"`
}

// ScreenshotAssertion captures a screenshot; Save names the artifact file
// (per run: <save>-run<N>.png under the report dir).
type ScreenshotAssertion struct {
	Save string `yaml:"save,omitempty" json:"save,omitempty"`
}

// LoadScenario parses a scenario from a YAML or JSON file (YAML is a
// superset of JSON, so both parse with the same decoder) and validates it.
func LoadScenario(path string) (*Scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Scenario
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // reject unknown/misspelled fields
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// Validate checks structural invariants of the scenario.
func (s *Scenario) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("scenario has no name")
	}
	if s.Name != filepath.Base(s.Name) {
		return fmt.Errorf("scenario name %q must be a plain filename component", s.Name)
	}
	if len(s.Lease.Labels) == 0 && s.Lease.UDID == "" {
		return fmt.Errorf("scenario %q: lease needs labels or a udid pin", s.Name)
	}
	if len(s.Steps) == 0 {
		return fmt.Errorf("scenario %q: at least one step is required", s.Name)
	}
	seen := map[string]int{}
	for i := range s.Steps {
		st := &s.Steps[i]
		if err := st.validate(); err != nil {
			return fmt.Errorf("scenario %q step %d (%s): %w", s.Name, i+1, st.Label(i), err)
		}
		// Report aggregation is keyed by step label; duplicates would
		// silently merge two steps into one row.
		label := st.Label(i)
		if prev, ok := seen[label]; ok {
			return fmt.Errorf("scenario %q: steps %d and %d have the same name %q", s.Name, prev, i+1, label)
		}
		seen[label] = i + 1
	}
	return nil
}

func (st *Step) validate() error {
	switch st.Op {
	case "boot", "shutdown", "erase":
	case "action":
		if st.Kind == "" {
			return fmt.Errorf("action step needs kind")
		}
	case "fixture":
		if st.Fixture == "" {
			return fmt.Errorf("fixture step needs fixture name")
		}
	case "snapshot":
		if st.SnapshotLabel == "" {
			return fmt.Errorf("snapshot step needs snapshot_label")
		}
	case "restore":
		if st.Snapshot == "" {
			return fmt.Errorf("restore step needs snapshot")
		}
	case "wait":
		if st.Duration <= 0 {
			return fmt.Errorf("wait step needs a positive duration")
		}
		if st.Timeout > 0 && st.Timeout <= st.Duration {
			return fmt.Errorf("wait step timeout %s must exceed its duration %s", st.Timeout.Std(), st.Duration.Std())
		}
	case "assert":
		if st.Assert == nil {
			return fmt.Errorf("assert step needs an assert block")
		}
		n := 0
		if st.Assert.ElementExists != nil {
			n++
		}
		if st.Assert.ElementAbsent != nil {
			n++
		}
		if st.Assert.TreeHash != nil {
			n++
		}
		if st.Assert.Screenshot != nil {
			n++
		}
		if n != 1 {
			return fmt.Errorf("assert step needs exactly one assertion, got %d", n)
		}
		if q := st.Assert.ElementExists; q != nil && q.empty() {
			return fmt.Errorf("element_exists query needs role, label, or value")
		}
		if q := st.Assert.ElementAbsent; q != nil && q.empty() {
			return fmt.Errorf("element_absent query needs role, label, or value")
		}
		if sc := st.Assert.Screenshot; sc != nil && sc.Save != "" && sc.Save != filepath.Base(sc.Save) {
			return fmt.Errorf("screenshot save %q must be a plain filename component", sc.Save)
		}
		if th := st.Assert.TreeHash; th != nil {
			set := 0
			if th.SaveAs != "" {
				set++
			}
			if th.EqualsSaved != "" {
				set++
			}
			if th.Equals != "" {
				set++
			}
			if set != 1 {
				return fmt.Errorf("tree_hash assertion needs exactly one of save_as, equals_saved, or equals, got %d", set)
			}
		}
	case "":
		return fmt.Errorf("step needs an op")
	default:
		return fmt.Errorf("unknown op %q", st.Op)
	}
	return nil
}

// Label returns a human-readable identifier for the step at index i.
func (st *Step) Label(i int) string {
	if st.Name != "" {
		return st.Name
	}
	if st.Op == "action" {
		return fmt.Sprintf("%02d-%s-%s", i+1, st.Op, st.Kind)
	}
	return fmt.Sprintf("%02d-%s", i+1, st.Op)
}
