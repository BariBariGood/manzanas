package journal

import (
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/runspec"
	"github.com/BariBariGood/manzanas/proto"
)

func specEntries() []Entry {
	mk := func(seq int64, kind string, payload map[string]any) Entry {
		return Entry{Ref: proto.JournalRef{RunID: "lse_spec1", Seq: seq}, Kind: kind, Payload: payload}
	}
	return []Entry{
		mk(1, "lease", map[string]any{"action": "leases.acquire", "status": "ok"}),
		mk(2, "action", map[string]any{"action": "targets.boot", "status": "ok",
			"params": map[string]any{"udid": "MOCK-1"}}),
		mk(3, "action", map[string]any{"action": "observe", "status": "ok"}),
		mk(4, "action", map[string]any{"action": "type_into_element", "status": "ok",
			"params": map[string]any{"id": "username", "text": "agent"}}),
		mk(5, "action", map[string]any{"action": "tap", "status": "ok",
			"params": map[string]any{"x": float64(100), "y": float64(200)}}),
		mk(6, "observation", map[string]any{"action": "screenshot", "status": "ok"}),
		mk(7, "action", map[string]any{"action": "screenshot", "status": "ok",
			"params": map[string]any{"inline": false}}),
		mk(8, "action", map[string]any{"action": "tap_element", "status": "error",
			"error": "element not found", "params": map[string]any{"label": "Nope"}}),
		mk(9, "action", map[string]any{"action": "button", "status": "ok",
			"params": map[string]any{"name": "home"}}),
	}
}

func TestBuildSpecDefaults(t *testing.T) {
	meta := RunMeta{RunID: "lse_spec1", Purpose: "login flow",
		TargetUDID: "MOCK-1", Runtime: "iOS 26.0", DeviceType: "iPhone 17 Pro"}
	spec := BuildSpec(meta, specEntries(), SpecOptions{})

	if spec.Name != "login flow" {
		t.Fatalf("name = %q", spec.Name)
	}
	if spec.Target.Runtime != "iOS 26.0" || spec.Target.DeviceType != "iPhone 17 Pro" || spec.Target.UDID != "" {
		t.Fatalf("target = %+v", spec.Target)
	}
	// Dropped: lease/observation kinds, dotted lifecycle methods
	// (targets.boot), pure reads (observe, screenshot), the failed step
	// and everything after it.
	got := make([]string, 0, len(spec.Steps))
	for _, s := range spec.Steps {
		got = append(got, s.Action)
	}
	want := []string{"type_into_element", "tap"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	if spec.Steps[0].With["text"] != "agent" || spec.Steps[1].With["x"] != float64(100) {
		t.Fatalf("params not passed through: %+v", spec.Steps)
	}
}

func TestBuildSpecKeepObservationsAndFailed(t *testing.T) {
	meta := RunMeta{RunID: "lse_spec1", TargetUDID: "MOCK-1"}
	spec := BuildSpec(meta, specEntries(), SpecOptions{KeepObservations: true, IncludeFailed: true})

	if spec.Name != "replay of lse_spec1" {
		t.Fatalf("name = %q", spec.Name)
	}
	// No runtime/device type in meta: falls back to the UDID pin.
	if spec.Target.UDID != "MOCK-1" || spec.Target.Runtime != "" {
		t.Fatalf("target = %+v", spec.Target)
	}
	got := make([]string, 0, len(spec.Steps))
	for _, s := range spec.Steps {
		got = append(got, s.Action)
	}
	want := []string{"observe", "type_into_element", "tap", "screenshot", "tap_element", "button"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v, want %v", got, want)
	}
}

func TestBuildSpecFailedReadTruncates(t *testing.T) {
	// A failed pure-read entry (e.g. wait_for_element timing out) must
	// still truncate the spec — the read-only drop must not skip past it.
	entries := []Entry{
		{Kind: "action", Payload: map[string]any{"action": "tap", "status": "ok",
			"params": map[string]any{"x": float64(1), "y": float64(2)}}},
		{Kind: "action", Payload: map[string]any{"action": "wait_for_element",
			"status": "error", "error": "timeout"}},
		{Kind: "action", Payload: map[string]any{"action": "button", "status": "ok",
			"params": map[string]any{"name": "home"}}},
	}
	spec := BuildSpec(RunMeta{TargetUDID: "MOCK-1"}, entries, SpecOptions{})
	if len(spec.Steps) != 1 || spec.Steps[0].Action != "tap" {
		t.Fatalf("steps = %+v, want just the tap before the failed wait", spec.Steps)
	}
}

func TestBuildSpecWithExpectations(t *testing.T) {
	mk := func(seq int64, kind string, payload map[string]any) Entry {
		return Entry{Ref: proto.JournalRef{RunID: "lse_exp1", Seq: seq}, Kind: kind, Payload: payload}
	}
	entries := []Entry{
		mk(1, "action", map[string]any{"action": "tap_element", "status": "ok",
			"params": map[string]any{"id": "username"}}),
		mk(2, "observation", map[string]any{"action": StepExpectationAction, "status": "ok",
			"step_index": 0, "texts": []any{"mock login", "username"}, "dhash": "a5a5a5a5a5a5a5a5"}),
		mk(3, "action", map[string]any{"action": "tap", "status": "ok",
			"params": map[string]any{"x": float64(1), "y": float64(2)}}),
		// Observe failed at record time: DHash only.
		mk(4, "observation", map[string]any{"action": StepExpectationAction, "status": "ok",
			"step_index": 1, "texts_error": "observe failed", "dhash": "b6b6b6b6b6b6b6b6"}),
		mk(5, "action", map[string]any{"action": "button", "status": "ok",
			"params": map[string]any{"name": "home"}}),
		// Nothing captured: no expect block.
		mk(6, "observation", map[string]any{"action": StepExpectationAction, "status": "ok",
			"step_index": 2, "texts_error": "observe failed", "dhash_error": "screenshot failed"}),
	}
	meta := RunMeta{RunID: "lse_exp1", TargetUDID: "MOCK-1"}

	// Without the option the observation entries are ignored.
	plain := BuildSpec(meta, entries, SpecOptions{})
	for i, st := range plain.Steps {
		if st.Expect != nil {
			t.Fatalf("steps[%d] has expect without WithExpectations", i)
		}
	}

	spec := BuildSpec(meta, entries, SpecOptions{WithExpectations: true})
	if len(spec.Steps) != 3 {
		t.Fatalf("steps = %+v", spec.Steps)
	}
	e0 := spec.Steps[0].Expect
	if e0 == nil || strings.Join(e0.Texts, ",") != "mock login,username" || e0.Fingerprint != "a5a5a5a5a5a5a5a5" {
		t.Fatalf("steps[0].expect = %+v", e0)
	}
	e1 := spec.Steps[1].Expect
	if e1 == nil || len(e1.Texts) != 0 || e1.Fingerprint != "b6b6b6b6b6b6b6b6" {
		t.Fatalf("steps[1].expect = %+v", e1)
	}
	if spec.Steps[2].Expect != nil {
		t.Fatalf("steps[2].expect = %+v, want nil (nothing captured)", spec.Steps[2].Expect)
	}

	// The exported YAML still round-trips through the strict parser.
	out, err := ExportSpecYAML(meta, entries, SpecOptions{WithExpectations: true})
	if err != nil {
		t.Fatal(err)
	}
	reparsed, err := runspec.Parse(out)
	if err != nil {
		t.Fatalf("round-trip: %v\n%s", err, out)
	}
	if reparsed.Steps[0].Expect == nil || reparsed.Steps[0].Expect.Fingerprint != "a5a5a5a5a5a5a5a5" {
		t.Fatalf("round-trip lost expect: %+v", reparsed.Steps)
	}
}

// The expectation recorder's own observe/screenshot dispatches are
// journaled as read-only action entries between the mutating step and its
// step-expectation observation; the expect block must attach to the
// mutating step even when KeepObservations exports those entries as steps.
func TestBuildSpecWithExpectationsKeepObservations(t *testing.T) {
	mk := func(seq int64, kind string, payload map[string]any) Entry {
		return Entry{Ref: proto.JournalRef{RunID: "lse_exp2", Seq: seq}, Kind: kind, Payload: payload}
	}
	entries := []Entry{
		mk(1, "action", map[string]any{"action": "tap_element", "status": "ok",
			"params": map[string]any{"id": "username"}}),
		// The recorder's own captures carry the purpose marker; the
		// screenshot failed at record time — neither is exported and the
		// failure must not truncate the spec.
		mk(2, "action", map[string]any{"action": "observe", "status": "ok",
			"params": map[string]any{"purpose": StepExpectationAction}}),
		mk(3, "action", map[string]any{"action": "screenshot", "status": "error",
			"params": map[string]any{"purpose": StepExpectationAction}}),
		mk(4, "action", map[string]any{"action": "observe", "status": "ok"}),
		mk(5, "action", map[string]any{"action": "screenshot", "status": "ok"}),
		mk(6, "observation", map[string]any{"action": StepExpectationAction, "status": "ok",
			"step_index": 0, "texts": []any{"username"}, "dhash": "a5a5a5a5a5a5a5a5"}),
	}
	spec := BuildSpec(RunMeta{RunID: "lse_exp2", TargetUDID: "MOCK-1"}, entries,
		SpecOptions{WithExpectations: true, KeepObservations: true})
	if len(spec.Steps) != 3 {
		t.Fatalf("steps = %+v", spec.Steps)
	}
	if spec.Steps[0].Expect == nil || spec.Steps[0].Expect.Fingerprint != "a5a5a5a5a5a5a5a5" {
		t.Fatalf("steps[0].expect = %+v, want the recorded expectation", spec.Steps[0].Expect)
	}
	if spec.Steps[1].Expect != nil || spec.Steps[2].Expect != nil {
		t.Fatalf("expect attached to an auxiliary read-only step: %+v", spec.Steps)
	}
}

func TestExportSpecYAMLRoundTrips(t *testing.T) {
	meta := RunMeta{RunID: "lse_spec1", Purpose: "login flow",
		Runtime: "iOS 26.0", DeviceType: "iPhone 17 Pro"}
	out, err := ExportSpecYAML(meta, specEntries(), SpecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := runspec.Parse(out)
	if err != nil {
		t.Fatalf("emitted YAML rejected by strict parser: %v\n%s", err, out)
	}
	if len(parsed.Steps) != 2 || parsed.Steps[0].Action != "type_into_element" {
		t.Fatalf("round-trip lost steps: %+v", parsed.Steps)
	}
	if parsed.Steps[0].With["id"] != "username" {
		t.Fatalf("round-trip lost params: %+v", parsed.Steps[0].With)
	}
}

func TestExportSpecYAMLStateWarning(t *testing.T) {
	entries := append(specEntries(),
		Entry{Kind: "state", Payload: map[string]any{"action": "state.fixture",
			"status": "ok", "params": map[string]any{"name": "seed-users"}}},
		Entry{Kind: "state", Payload: map[string]any{"action": "state.fixture", "status": "ok"}},
		Entry{Kind: "state", Payload: map[string]any{"action": "state.erase", "status": "ok"}},
	)
	out, err := ExportSpecYAML(RunMeta{RunID: "lse_spec1", TargetUDID: "MOCK-1"}, entries, SpecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "# WARNING:") ||
		!strings.Contains(string(out), "state.fixture, state.erase") {
		t.Fatalf("missing state-op warning header:\n%s", out)
	}
	// The comment header must not break the strict parser.
	if _, err := runspec.Parse(out); err != nil {
		t.Fatalf("warned YAML rejected: %v", err)
	}
}

func TestExportSpecYAMLInvalidTarget(t *testing.T) {
	// A run with no target meta at all cannot produce a valid spec; the
	// exporter must fail loudly instead of emitting an unrunnable file.
	if _, err := ExportSpecYAML(RunMeta{RunID: "lse_x"}, specEntries(), SpecOptions{}); err == nil {
		t.Fatal("expected round-trip validation error for empty target")
	}
}
