package journal

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/BariBariGood/manzanas/internal/runspec"
	"github.com/BariBariGood/manzanas/proto"
)

// SpecOptions controls how a journaled run is turned back into a run-spec
// (`manzanas journal to-spec`, docs/journal.md).
type SpecOptions struct {
	// KeepObservations keeps pure-read action kinds (observe, screenshot,
	// audit, wait_for_element, ...) as steps instead of dropping them.
	KeepObservations bool
	// IncludeFailed keeps the first failed step and everything after it;
	// by default the spec stops just before the first failure.
	IncludeFailed bool
	// WithExpectations attaches expect blocks (masked text set + DHash
	// fingerprint) to exported steps from the run's recorded
	// step-expectation observations (runs executed with
	// record_expectations: true), so the replay detects drift.
	WithExpectations bool
}

// StepExpectationAction is the journal action name of the post-step
// observation entries the runs engine records under
// record_expectations: true (kind "observation"): the masked text set
// ("texts") and screen DHash ("dhash") the exporter turns back into a
// replay step's expect block.
const StepExpectationAction = "runs.step_expectation"

// readOnlyKinds are the pure-read action kinds a replay does not need to
// re-issue: they observe state without mutating it. Dropped by default,
// kept with SpecOptions.KeepObservations (useful as verification anchors).
var readOnlyKinds = map[string]struct{}{
	"observe":          {},
	"ui_tree":          {},
	"screenshot":       {},
	"audit":            {},
	"pasteboard_get":   {},
	"wait_for_element": {},
	"wait_tree_stable": {},
	// Assertions observe a postcondition without mutating state. Note
	// assert_template's inline template_base64 is elided from the journal
	// (see ElideBlob), so such a step is only exportable when it names a
	// stored template.
	"assert_text":     {},
	"assert_no_text":  {},
	"assert_template": {},
	"wait_for_stable": {},
}

// ReadOnlyKind reports whether an action kind is a pure read (observes
// state without mutating it). The runs engine uses the same
// classification for verdict aggregation (PROTOCOL.md §9).
func ReadOnlyKind(action string) bool {
	_, ok := readOnlyKinds[action]
	return ok
}

// ElideBlob is the placeholder journaled in place of a bulky inline
// binary payload field (an entry over the journal's line cap would be
// dropped from reads entirely).
func ElideBlob(n int) string { return fmt.Sprintf("<%d base64 bytes elided>", n) }

// IsElidedBlob reports whether a journaled string param is an ElideBlob
// placeholder rather than the original payload value.
func IsElidedBlob(s string) bool {
	return strings.HasPrefix(s, "<") && strings.HasSuffix(s, " base64 bytes elided>")
}

// BuildSpec maps a journaled run back to a declarative run-spec: each
// seq-ordered kind:"action" entry whose action is a PROTOCOL.md §5 action
// kind becomes a RunStep{Action, With: params}, and the run's determinism
// meta becomes the target requirement. Protocol methods journaled as
// action entries (dotted names like targets.boot or leases.acquire) are
// lifecycle the runs engine re-performs itself, so they are never emitted
// as steps. Coordinate-only taps are exported as-is.
func BuildSpec(meta RunMeta, entries []Entry, opts SpecOptions) proto.RunSpec {
	spec := proto.RunSpec{
		Name:   specName(meta),
		Target: specTarget(meta),
	}
	// lastMutating is the index in spec.Steps of the most recently
	// exported mutating step — the step a runs.step_expectation
	// observation describes. Expectations are recorded only after
	// mutating steps, and the recorder's own observe/screenshot
	// dispatches journal in between as read-only action entries, so
	// attachment must skip past those (and any read-only steps kept by
	// KeepObservations).
	lastMutating := -1
	for _, e := range entries {
		if opts.WithExpectations && e.Kind == "observation" &&
			payloadString(e.Payload, "action") == StepExpectationAction &&
			lastMutating >= 0 {
			if exp := expectFromEntry(e.Payload); exp != nil {
				spec.Steps[lastMutating].Expect = exp
			}
			continue
		}
		if e.Kind != "action" {
			continue
		}
		action := payloadString(e.Payload, "action")
		if action == "" || strings.Contains(action, ".") {
			continue
		}
		if params, ok := e.Payload["params"].(map[string]any); ok {
			// Engine-injected expectation/drift captures are not user
			// steps: never export them, and never let a failed
			// best-effort capture truncate the exported spec.
			if p, _ := params["purpose"].(string); p == StepExpectationAction {
				continue
			}
		}
		failed := payloadString(e.Payload, "status") == "error"
		if failed && !opts.IncludeFailed {
			break
		}
		if _, readOnly := readOnlyKinds[action]; readOnly && !opts.KeepObservations {
			continue
		}
		step := proto.RunStep{Action: action}
		if params, ok := e.Payload["params"].(map[string]any); ok && len(params) > 0 {
			// An elided inline template cannot be re-issued: the journal
			// holds a placeholder, not the image. Skip the step rather
			// than export a spec the daemon would reject.
			if b64, ok := params["template_base64"].(string); ok && IsElidedBlob(b64) {
				if !ReadOnlyKind(action) {
					// The dropped step's expectation must not attach to
					// an earlier step.
					lastMutating = -1
				}
				continue
			}
			step.With = params
		}
		spec.Steps = append(spec.Steps, step)
		if !ReadOnlyKind(action) {
			lastMutating = len(spec.Steps) - 1
		}
	}
	return spec
}

// ExportSpecYAML builds the run-spec for a journaled run and returns it as
// YAML, guaranteed to round-trip: the emitted document is re-parsed through
// the strict run-spec parser (runspec.Parse, KnownFields + validation)
// before being returned, so `manzanas run` accepts it verbatim. State ops
// the run performed (fixtures, snapshot/restore/erase) cannot be
// reconstructed — the journal records their names but not their payloads —
// so when any were recorded the emitted file carries a warning header
// listing what was dropped.
func ExportSpecYAML(meta RunMeta, entries []Entry, opts SpecOptions) ([]byte, error) {
	spec := BuildSpec(meta, entries, opts)
	out, err := yaml.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshal run-spec: %w", err)
	}
	if warn := stateWarning(entries); warn != "" {
		out = append([]byte(warn), out...)
	}
	if _, err := runspec.Parse(out); err != nil {
		return nil, fmt.Errorf("exported spec does not round-trip: %w", err)
	}
	return out, nil
}

// expectFromEntry maps a step-expectation observation payload to a
// replay expect block. Nil when the observation captured nothing usable
// (both the observe and the screenshot failed at record time).
func expectFromEntry(payload map[string]any) *proto.RunExpect {
	exp := &proto.RunExpect{}
	switch raw := payload["texts"].(type) {
	case []any:
		for _, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				exp.Texts = append(exp.Texts, s)
			}
		}
	case []string:
		// In-process entries (not yet round-tripped through JSON) keep
		// the typed slice.
		exp.Texts = append(exp.Texts, raw...)
	}
	exp.Fingerprint = payloadString(payload, "dhash")
	if len(exp.Texts) == 0 && exp.Fingerprint == "" {
		return nil
	}
	return exp
}

// stateWarning renders the YAML comment header flagging recorded state ops
// a replay will not re-perform; empty when the run recorded none.
func stateWarning(entries []Entry) string {
	var ops []string
	seen := map[string]struct{}{}
	for _, e := range entries {
		if e.Kind != "state" {
			continue
		}
		op := payloadString(e.Payload, "action")
		if op == "" {
			op = "state op"
		}
		if _, dup := seen[op]; dup {
			continue
		}
		seen[op] = struct{}{}
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return ""
	}
	return fmt.Sprintf("# WARNING: the recorded run performed state ops (%s) that are NOT\n"+
		"# reconstructed here (the journal records their names, not their payloads).\n"+
		"# Re-seed equivalent state (target.fixtures / target.reset) before replaying.\n",
		strings.Join(ops, ", "))
}

// specName labels the replay spec: the recorded purpose when there is one,
// else a reference to the source run.
func specName(meta RunMeta) string {
	if meta.Purpose != "" {
		return meta.Purpose
	}
	if meta.RunID != "" {
		return "replay of " + meta.RunID
	}
	return ""
}

// specTarget maps the run's determinism meta to a target requirement.
// Runtime + device type are preferred (portable across hosts); a UDID pin
// is the fallback when the meta recorded neither.
func specTarget(meta RunMeta) proto.RunTarget {
	if meta.Runtime != "" || meta.DeviceType != "" {
		return proto.RunTarget{Runtime: meta.Runtime, DeviceType: meta.DeviceType}
	}
	return proto.RunTarget{UDID: meta.TargetUDID}
}
