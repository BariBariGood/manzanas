package actions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// seqRunner replays a fixed sequence of describe-ui outputs; the last entry
// repeats once the sequence is exhausted. Non-describe-ui calls succeed
// with empty output.
type seqRunner struct {
	mu      sync.Mutex
	outputs []seqStep
	calls   int
}

type seqStep struct {
	stdout string
	err    string // non-empty means exit status 1 with this stderr
}

func (s *seqRunner) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	if len(args) == 0 || args[0] != "describe-ui" {
		return nil, nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	if i >= len(s.outputs) {
		i = len(s.outputs) - 1
	}
	s.calls++
	step := s.outputs[i]
	if step.err != "" {
		return nil, []byte(step.err), fmt.Errorf("exit status 1")
	}
	return []byte(step.stdout), nil, nil
}

func seqBackend(steps ...seqStep) (*AXeBackend, *seqRunner) {
	r := &seqRunner{outputs: steps}
	return NewAXe(WithRunner(r), WithAXePath("/fake/axe")), r
}

// treeJSON builds a minimal describe-ui JSON document with one cell per label.
func treeJSON(labels ...string) string {
	cells := make([]string, 0, len(labels))
	for i, l := range labels {
		cells = append(cells, fmt.Sprintf(
			`{"role":"Cell","AXLabel":%q,"frame":{"x":0,"y":%d,"width":400,"height":44}}`, l, 100+i*44))
	}
	return `[{"role":"Window","children":[` + strings.Join(cells, ",") + `]}]`
}

func fastWait(extra map[string]any) map[string]any {
	p := map[string]any{"timeout_ms": 300.0, "interval_ms": 10.0}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func TestWaitForElementAppears(t *testing.T) {
	b, r := seqBackend(
		seqStep{stdout: treeJSON("Settings")},
		seqStep{stdout: treeJSON("Settings")},
		seqStep{stdout: treeJSON("Settings", "General")},
	)
	res, err := handleWaitForElement(context.Background(), b, "UDID", fastWait(map[string]any{"label": "General"}))
	if err != nil {
		t.Fatalf("wait_for_element: %v", err)
	}
	el, ok := res["element"].(*Node)
	if !ok || el.Label != "General" || el.Frame == nil {
		t.Fatalf("bad element: %+v", res["element"])
	}
	if el.Children != nil {
		t.Fatalf("element should not carry children")
	}
	if polls := res["polls"].(int); polls != 3 {
		t.Fatalf("polls = %d, want 3", polls)
	}
	if r.calls != 3 {
		t.Fatalf("describe-ui calls = %d, want 3", r.calls)
	}
}

func TestWaitForElementAbsent(t *testing.T) {
	b, _ := seqBackend(
		seqStep{stdout: treeJSON("Spinner")},
		seqStep{stdout: treeJSON("Done")},
	)
	res, err := handleWaitForElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "Spinner", "absent": true}))
	if err != nil {
		t.Fatalf("wait_for_element absent: %v", err)
	}
	if res["absent"] != true {
		t.Fatalf("expected absent:true, got %+v", res)
	}
}

func TestWaitForElementTimeout(t *testing.T) {
	b, _ := seqBackend(seqStep{stdout: treeJSON("Settings")})
	_, err := handleWaitForElement(context.Background(), b, "UDID",
		map[string]any{"label": "Nope", "timeout_ms": 50.0, "interval_ms": 10.0})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "timeout" {
		t.Fatalf("want timeout error, got %v", err)
	}
	if !strings.Contains(ae.Message, `label="Nope"`) {
		t.Fatalf("timeout message should name the predicate: %q", ae.Message)
	}
}

func TestWaitForElementRidesOutTransientErrors(t *testing.T) {
	b, _ := seqBackend(
		seqStep{err: "No translation object returned for AXTraits"},
		seqStep{stdout: treeJSON("General")},
	)
	res, err := handleWaitForElement(context.Background(), b, "UDID", fastWait(map[string]any{"label": "General"}))
	if err != nil {
		t.Fatalf("wait_for_element: %v", err)
	}
	if res["element"].(*Node).Label != "General" {
		t.Fatalf("bad element: %+v", res["element"])
	}
}

func TestWaitRidesOutEmptySkeletonTree(t *testing.T) {
	// Right after launch describe-ui can return a skeleton tree that
	// compacts to nothing; that must not satisfy stability or absence.
	b, r := seqBackend(
		seqStep{stdout: `[{"role":"Window"}]`},
		seqStep{stdout: `[{"role":"Window"}]`},
		seqStep{stdout: `[{"role":"Window"}]`},
		seqStep{stdout: treeJSON("Loaded")},
		seqStep{stdout: treeJSON("Loaded")},
		seqStep{stdout: treeJSON("Loaded")},
	)
	res, err := handleWaitTreeStable(context.Background(), b, "UDID", fastWait(nil))
	if err != nil {
		t.Fatalf("wait_tree_stable: %v", err)
	}
	if res["stable"] != true {
		t.Fatalf("stable = %v, want true", res["stable"])
	}
	wantNodes, _ := CompactTree([]byte(treeJSON("Loaded")))
	if res["hash"] != TreeHash(wantNodes) {
		t.Fatalf("hash = %v, want hash of loaded tree (empty skeletons must not settle)", res["hash"])
	}
	if r.calls != 6 {
		t.Fatalf("describe-ui calls = %d, want 6", r.calls)
	}

	b2, _ := seqBackend(
		seqStep{stdout: `[{"role":"Window"}]`},
		seqStep{stdout: treeJSON("Spinner")},
	)
	_, err = handleWaitForElement(context.Background(), b2, "UDID",
		map[string]any{"label": "Spinner", "absent": true, "timeout_ms": 60.0, "interval_ms": 10.0})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "timeout" {
		t.Fatalf("absent must not be satisfied by a skeleton tree; got %v", err)
	}
}

func TestWaitForElementHardErrorPropagates(t *testing.T) {
	b, _ := seqBackend(seqStep{err: "Invalid device: UDID"})
	_, err := handleWaitForElement(context.Background(), b, "UDID", fastWait(map[string]any{"label": "x"}))
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "internal" {
		t.Fatalf("want internal error, got %v", err)
	}
}

func TestWaitForElementRequiresPredicate(t *testing.T) {
	b, _ := seqBackend(seqStep{stdout: treeJSON()})
	_, err := handleWaitForElement(context.Background(), b, "UDID", map[string]any{})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "bad_request" {
		t.Fatalf("want bad_request, got %v", err)
	}
}

func TestWaitTreeStableSettles(t *testing.T) {
	b, r := seqBackend(
		seqStep{stdout: treeJSON("Loading")},
		seqStep{stdout: treeJSON("Loading", "More")},
		seqStep{stdout: treeJSON("Final")},
		seqStep{stdout: treeJSON("Final")},
		seqStep{stdout: treeJSON("Final")},
	)
	res, err := handleWaitTreeStable(context.Background(), b, "UDID", fastWait(nil))
	if err != nil {
		t.Fatalf("wait_tree_stable: %v", err)
	}
	if res["stable"] != true {
		t.Fatalf("stable = %v, want true", res["stable"])
	}
	if r.calls != 5 {
		t.Fatalf("describe-ui calls = %d, want 5", r.calls)
	}
	wantNodes, _ := CompactTree([]byte(treeJSON("Final")))
	if res["hash"] != TreeHash(wantNodes) {
		t.Fatalf("hash = %v, want hash of final tree", res["hash"])
	}
	if res["samples"].(int) != 5 {
		t.Fatalf("samples = %v, want 5", res["samples"])
	}
}

func TestWaitTreeStableCustomSamples(t *testing.T) {
	b, r := seqBackend(seqStep{stdout: treeJSON("Static")})
	if _, err := handleWaitTreeStable(context.Background(), b, "UDID",
		fastWait(map[string]any{"stable_samples": 2.0})); err != nil {
		t.Fatalf("wait_tree_stable: %v", err)
	}
	if r.calls != 2 {
		t.Fatalf("describe-ui calls = %d, want 2", r.calls)
	}
}

func TestWaitTreeStableUnstable(t *testing.T) {
	// Every poll returns a different tree (a continuously-animating
	// screen), so stability is never reached: the max-wait elapses and
	// the call succeeds with a distinct stable:false result instead of a
	// generic timeout error.
	steps := make([]seqStep, 0, 64)
	for i := 0; i < 64; i++ {
		steps = append(steps, seqStep{stdout: treeJSON(fmt.Sprintf("Frame %d", i))})
	}
	b, _ := seqBackend(steps...)
	res, err := handleWaitTreeStable(context.Background(), b, "UDID",
		map[string]any{"timeout_ms": 60.0, "interval_ms": 10.0})
	if err != nil {
		t.Fatalf("animating screen: %v, want a stable:false result", err)
	}
	if res["stable"] != false {
		t.Fatalf("stable = %v, want false", res["stable"])
	}
	if res["hash"] == "" {
		t.Fatalf("unstable result should still carry the last observed hash")
	}
	if n, _ := res["samples"].(int); n < 2 {
		t.Fatalf("samples = %v, want >= 2", res["samples"])
	}
}

func TestWaitTreeStableUnstableSurvivesTrailingGlitch(t *testing.T) {
	// An animating screen whose final polls glitch transiently must still
	// report stable:false with the last good hash — only a tree that was
	// NEVER readable is an error.
	steps := make([]seqStep, 0, 64)
	for i := 0; i < 4; i++ {
		steps = append(steps, seqStep{stdout: treeJSON(fmt.Sprintf("Frame %d", i))})
	}
	for i := 0; i < 60; i++ {
		steps = append(steps, seqStep{err: "No translation object returned for AXTraits"})
	}
	b, _ := seqBackend(steps...)
	res, err := handleWaitTreeStable(context.Background(), b, "UDID",
		map[string]any{"timeout_ms": 60.0, "interval_ms": 10.0})
	if err != nil {
		t.Fatalf("trailing glitch: %v, want a stable:false result", err)
	}
	if res["stable"] != false || res["hash"] == "" {
		t.Fatalf("res = %+v, want stable:false with the last good hash", res)
	}
}

func TestWaitTreeStableNeverReadableIsError(t *testing.T) {
	// Every poll fails transiently (a11y bridge never attaches): no tree was
	// ever observed, so this is a real failure, not a stable:false "live UI".
	steps := make([]seqStep, 0, 64)
	for i := 0; i < 64; i++ {
		steps = append(steps, seqStep{err: "No translation object returned for AXTraits"})
	}
	b, _ := seqBackend(steps...)
	_, err := handleWaitTreeStable(context.Background(), b, "UDID",
		map[string]any{"timeout_ms": 60.0, "interval_ms": 10.0})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "timeout" {
		t.Fatalf("want timeout error for a never-readable tree, got %v", err)
	}
}

func TestWaitTreeStableRequireStable(t *testing.T) {
	// Callers that need a settled tree (deterministic hash capture) opt
	// into the hard timeout.
	steps := make([]seqStep, 0, 64)
	for i := 0; i < 64; i++ {
		steps = append(steps, seqStep{stdout: treeJSON(fmt.Sprintf("Frame %d", i))})
	}
	b, _ := seqBackend(steps...)
	_, err := handleWaitTreeStable(context.Background(), b, "UDID",
		map[string]any{"timeout_ms": 60.0, "interval_ms": 10.0, "require_stable": true})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "timeout" {
		t.Fatalf("want timeout error, got %v", err)
	}

	b2, _ := seqBackend(seqStep{stdout: treeJSON("x")})
	_, err = handleWaitTreeStable(context.Background(), b2, "UDID",
		fastWait(map[string]any{"require_stable": "yes"}))
	if !errors.As(err, &ae) || ae.Code != "bad_request" {
		t.Fatalf("non-boolean require_stable: want bad_request, got %v", err)
	}
}

func TestWaitTreeStableHardErrorStillFails(t *testing.T) {
	// A backend failure is still an error — only non-settling is soft.
	b, _ := seqBackend(seqStep{err: "Invalid device: UDID"})
	_, err := handleWaitTreeStable(context.Background(), b, "UDID", fastWait(nil))
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "internal" {
		t.Fatalf("want internal error, got %v", err)
	}
}

func TestWaitTreeStableRejectsBadSamples(t *testing.T) {
	b, _ := seqBackend(seqStep{stdout: treeJSON("x")})
	for _, v := range []any{1.0, 0.0, 21.0, "three"} {
		_, err := handleWaitTreeStable(context.Background(), b, "UDID",
			fastWait(map[string]any{"stable_samples": v}))
		var ae *Error
		if !errors.As(err, &ae) || ae.Code != "bad_request" {
			t.Fatalf("stable_samples=%v: want bad_request, got %v", v, err)
		}
	}
}

func TestWaitRejectsBadDurations(t *testing.T) {
	b, _ := seqBackend(seqStep{stdout: treeJSON("x")})
	for _, p := range []map[string]any{
		{"label": "x", "timeout_ms": -1.0},
		{"label": "x", "timeout_ms": "soon"},
		{"label": "x", "interval_ms": 0.0},
	} {
		_, err := handleWaitForElement(context.Background(), b, "UDID", p)
		var ae *Error
		if !errors.As(err, &ae) || ae.Code != "bad_request" {
			t.Fatalf("payload %v: want bad_request, got %v", p, err)
		}
	}
}
