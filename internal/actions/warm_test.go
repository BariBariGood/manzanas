package actions

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// coldFake records dispatches that reached the cold backend.
type coldFake struct {
	kinds []string
	err   error
}

func (c *coldFake) Dispatch(_ context.Context, _ string, req proto.ActionRequest) (proto.ActionResult, error) {
	c.kinds = append(c.kinds, req.Kind)
	if c.err != nil {
		return proto.ActionResult{}, c.err
	}
	return proto.ActionResult{OK: true, Result: map[string]any{"cold": true}}, nil
}

func warmFor(t *testing.T, ff *fakeFactory, cold *coldFake) *WarmBackend {
	t.Helper()
	b := NewWarm(cold, ff.factory, PoolConfig{})
	t.Cleanup(b.Close)
	return b
}

func TestWarmTapUsesHelperNotCold(t *testing.T) {
	ff := &fakeFactory{}
	cold := &coldFake{}
	b := warmFor(t, ff, cold)

	res, err := b.Dispatch(context.Background(), "U1",
		proto.ActionRequest{Kind: "tap", Payload: map[string]any{"x": 10, "y": 20}})
	if err != nil {
		t.Fatalf("tap: %v", err)
	}
	if !res.OK || res.Result["x"] != 10.0 || res.Result["y"] != 20.0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(cold.kinds) != 0 {
		t.Fatalf("cold backend was called: %v", cold.kinds)
	}
	if ff.spawnCount() != 1 || ff.helper(0).calls[0] != "tap" {
		t.Fatalf("helper not used: spawns=%d", ff.spawnCount())
	}
}

func TestWarmFallsBackColdWhenHelperUnavailable(t *testing.T) {
	ff := &fakeFactory{spawnErrs: 99}
	cold := &coldFake{}
	b := warmFor(t, ff, cold)

	res, err := b.Dispatch(context.Background(), "U1",
		proto.ActionRequest{Kind: "tap", Payload: map[string]any{"x": 1, "y": 2}})
	if err != nil {
		t.Fatalf("fallback should succeed: %v", err)
	}
	if res.Result["cold"] != true {
		t.Fatalf("expected cold result, got %+v", res)
	}
	if len(cold.kinds) != 1 || cold.kinds[0] != "tap" {
		t.Fatalf("cold dispatches: %v", cold.kinds)
	}
}

func TestWarmRoutesUnsupportedKindsCold(t *testing.T) {
	ff := &fakeFactory{}
	cold := &coldFake{}
	b := warmFor(t, ff, cold)

	for _, kind := range []string{"screenshot", "type", "install_app", "launch_app"} {
		if _, err := b.Dispatch(context.Background(), "U1", proto.ActionRequest{Kind: kind}); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	if ff.spawnCount() != 0 {
		t.Fatal("cold-only kinds must not spawn helpers")
	}
	if len(cold.kinds) != 4 {
		t.Fatalf("cold dispatches: %v", cold.kinds)
	}
}

func TestWarmVolumeButtonIsBadRequest(t *testing.T) {
	ff := &fakeFactory{}
	cold := &coldFake{}
	b := warmFor(t, ff, cold)

	_, err := b.Dispatch(context.Background(), "U1",
		proto.ActionRequest{Kind: "button", Payload: map[string]any{"name": "volume-up"}})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != proto.ErrBadRequest {
		t.Fatalf("want bad_request for unsupported button, got %v", err)
	}
	if len(cold.kinds) != 0 {
		t.Fatalf("unsupported button must not be retried cold: %v", cold.kinds)
	}
	if ff.spawnCount() != 0 {
		t.Fatal("unsupported button must not spawn a helper")
	}
}

func TestWarmBadPayloadIsNotFallenBack(t *testing.T) {
	ff := &fakeFactory{}
	cold := &coldFake{}
	b := warmFor(t, ff, cold)

	_, err := b.Dispatch(context.Background(), "U1",
		proto.ActionRequest{Kind: "tap", Payload: map[string]any{"x": 5}})
	if err == nil {
		t.Fatal("missing y must fail")
	}
	if len(cold.kinds) != 0 {
		t.Fatal("validation errors must not be retried cold")
	}
}

func TestWarmObserveCompactsHelperTree(t *testing.T) {
	raw := `[{"AXLabel":"Login","type":"Button","frame":{"x":10,"y":20,"width":100,"height":40}}]`
	ff := &fakeFactory{configure: func(h *fakeHelper) {
		h.results["describe_ui"] = map[string]any{"raw": raw}
	}}
	cold := &coldFake{}
	b := warmFor(t, ff, cold)

	res, err := b.Dispatch(context.Background(), "U1",
		proto.ActionRequest{Kind: "observe", Payload: map[string]any{"include_raw": true}})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	nodes, ok := res.Result["tree"].([]*Node)
	if !ok || len(nodes) != 1 || nodes[0].Label != "Login" || nodes[0].Role != "Button" {
		t.Fatalf("unexpected tree: %+v", res.Result["tree"])
	}
	if res.Result["hash"] == "" {
		t.Fatal("missing tree hash")
	}
	if res.Result["raw"] != raw {
		t.Fatal("include_raw did not pass the raw tree through")
	}
	if len(cold.kinds) != 0 {
		t.Fatalf("observe must not go cold: %v", cold.kinds)
	}
}

func TestWarmObserveFallsBackColdOnTransportFailure(t *testing.T) {
	ff := &fakeFactory{configure: func(h *fakeHelper) { h.transportFails = 99 }}
	cold := &coldFake{}
	b := warmFor(t, ff, cold)

	res, err := b.Dispatch(context.Background(), "U1", proto.ActionRequest{Kind: "observe"})
	if err != nil {
		t.Fatalf("fallback should succeed: %v", err)
	}
	if res.Result["cold"] != true {
		t.Fatalf("expected cold result, got %+v", res)
	}
}

func TestWarmDeliveredTapIsNotRetriedCold(t *testing.T) {
	ff := &fakeFactory{configure: func(h *fakeHelper) { h.deliveredFails = 1 }}
	cold := &coldFake{}
	b := warmFor(t, ff, cold)

	_, err := b.Dispatch(context.Background(), "U1",
		proto.ActionRequest{Kind: "tap", Payload: map[string]any{"x": 1, "y": 2}})
	if err == nil {
		t.Fatal("delivered failure must surface")
	}
	if len(cold.kinds) != 0 {
		t.Fatalf("tap that may have landed must not be re-run cold: %v", cold.kinds)
	}
	if we := WireError(err); we.Code != "unavailable" {
		t.Fatalf("wire code %q", we.Code)
	}
}

func TestWarmDeliveredObserveFallsBackCold(t *testing.T) {
	ff := &fakeFactory{configure: func(h *fakeHelper) { h.deliveredFails = 99 }}
	cold := &coldFake{}
	b := warmFor(t, ff, cold)

	res, err := b.Dispatch(context.Background(), "U1", proto.ActionRequest{Kind: "observe"})
	if err != nil {
		t.Fatalf("observe is idempotent; fallback should succeed: %v", err)
	}
	if res.Result["cold"] != true {
		t.Fatalf("expected cold result, got %+v", res)
	}
}

func TestWarmTapAxHashes(t *testing.T) {
	raw := treeJSON("Login")
	ff := &fakeFactory{configure: func(h *fakeHelper) {
		h.results["describe_ui"] = map[string]any{"raw": raw}
	}}
	cold := &coldFake{}
	b := warmFor(t, ff, cold)

	res, err := b.Dispatch(context.Background(), "U1",
		proto.ActionRequest{Kind: "tap", Payload: map[string]any{"x": 10, "y": 20, "ax_hashes": true}})
	if err != nil {
		t.Fatalf("tap: %v", err)
	}
	before, _ := res.Result["ax_before"].(string)
	after, _ := res.Result["ax_after"].(string)
	if !strings.HasPrefix(before, "sha256:") || before != after {
		t.Fatalf("ax hashes: before=%q after=%q", before, after)
	}
	if res.Result["x"] != 10.0 || res.Result["y"] != 20.0 {
		t.Fatalf("tap result lost its fields: %+v", res.Result)
	}
	if len(cold.kinds) != 0 {
		t.Fatalf("ax_hashes must be served warm: %v", cold.kinds)
	}
	if calls := ff.helper(0).calls; len(calls) != 3 || calls[0] != "describe_ui" || calls[1] != "tap" || calls[2] != "describe_ui" {
		t.Fatalf("helper calls = %v, want [describe_ui tap describe_ui]", calls)
	}
}

func TestWarmAxHashSlowHashAbandonedWithoutKillingHelper(t *testing.T) {
	ff := &fakeFactory{configure: func(h *fakeHelper) {
		h.results["describe_ui"] = map[string]any{"raw": treeJSON("Login")}
		h.delays = map[string]time.Duration{"describe_ui": 200 * time.Millisecond}
	}}
	cold := &coldFake{}
	b := warmFor(t, ff, cold)
	old := warmAXHashTimeout
	warmAXHashTimeout = 20 * time.Millisecond
	defer func() { warmAXHashTimeout = old }()

	res, err := b.Dispatch(context.Background(), "U1",
		proto.ActionRequest{Kind: "tap", Payload: map[string]any{"x": 1, "y": 2, "ax_hashes": true}})
	if err != nil {
		t.Fatalf("tap: %v", err)
	}
	if _, ok := res.Result["ax_before"]; ok {
		t.Fatal("slow hash must be abandoned, not included")
	}
	if _, ok := res.Result["ax_after"]; ok {
		t.Fatal("slow hash must be abandoned, not included")
	}
	// The abandoned describe_ui must not tear the resident helper down.
	if _, err := b.Dispatch(context.Background(), "U1",
		proto.ActionRequest{Kind: "tap", Payload: map[string]any{"x": 3, "y": 4}}); err != nil {
		t.Fatalf("follow-up tap: %v", err)
	}
	if ff.spawnCount() != 1 {
		t.Fatalf("spawned %d helpers, want 1 (abandoned hash killed the helper)", ff.spawnCount())
	}
	if len(cold.kinds) != 0 {
		t.Fatalf("unexpected cold dispatches: %v", cold.kinds)
	}
}

func TestWarmAxHashesNonBoolRejected(t *testing.T) {
	ff := &fakeFactory{}
	cold := &coldFake{}
	b := warmFor(t, ff, cold)

	_, err := b.Dispatch(context.Background(), "U1",
		proto.ActionRequest{Kind: "tap", Payload: map[string]any{"x": 1, "y": 2, "ax_hashes": "yes"}})
	if we := WireError(err); we == nil || we.Code != "bad_request" {
		t.Fatalf("non-bool ax_hashes must be 400, got %v", err)
	}
	if ff.spawnCount() != 0 || len(cold.kinds) != 0 {
		t.Fatal("validation must run before any helper or cold dispatch")
	}
}

func TestWarmWaitPollsUseResidentHelper(t *testing.T) {
	ab, r := seqBackend(seqStep{stdout: treeJSON("ColdOnly")})
	ff := &fakeFactory{configure: func(h *fakeHelper) {
		h.results["describe_ui"] = map[string]any{"raw": treeJSON("Settings", "General")}
	}}
	b := NewWarm(ab, ff.factory, PoolConfig{})
	t.Cleanup(b.Close)

	res, err := b.Dispatch(context.Background(), "U1",
		proto.ActionRequest{Kind: "wait_for_element", Payload: fastWait(map[string]any{"label": "General"})})
	if err != nil {
		t.Fatalf("wait_for_element: %v", err)
	}
	el, ok := res.Result["element"].(*Node)
	if !ok || el.Label != "General" {
		t.Fatalf("bad element: %+v", res.Result["element"])
	}
	if r.calls != 0 {
		t.Fatalf("wait polls spawned the cold AXe CLI %d times; want 0", r.calls)
	}
	if calls := ff.helper(0).calls; len(calls) != 1 || calls[0] != "describe_ui" {
		t.Fatalf("helper calls = %v, want [describe_ui]", calls)
	}
}

func TestWarmWaitBudgetBoundsSlowPoll(t *testing.T) {
	ab, _ := seqBackend(seqStep{stdout: treeJSON("Nothing")})
	ff := &fakeFactory{configure: func(h *fakeHelper) {
		h.results["describe_ui"] = map[string]any{"raw": treeJSON("Nothing")}
		h.delays = map[string]time.Duration{"describe_ui": 2 * time.Second}
	}}
	b := NewWarm(ab, ff.factory, PoolConfig{})
	t.Cleanup(b.Close)

	start := time.Now()
	_, err := b.Dispatch(context.Background(), "U1",
		proto.ActionRequest{Kind: "wait_for_element", Payload: map[string]any{
			"label": "General", "timeout_ms": 100.0, "interval_ms": 10.0}})
	elapsed := time.Since(start)
	if we := WireError(err); we == nil || we.Code != "timeout" {
		t.Fatalf("want timeout, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("wait overshot its 100ms budget by %s", elapsed)
	}
}

func TestWarmWaitPollsFallBackColdOnTransportFailure(t *testing.T) {
	ab, r := seqBackend(seqStep{stdout: treeJSON("General")})
	ff := &fakeFactory{configure: func(h *fakeHelper) { h.transportFails = 99 }}
	b := NewWarm(ab, ff.factory, PoolConfig{})
	t.Cleanup(b.Close)

	res, err := b.Dispatch(context.Background(), "U1",
		proto.ActionRequest{Kind: "wait_for_element", Payload: fastWait(map[string]any{"label": "General"})})
	if err != nil {
		t.Fatalf("fallback poll should succeed: %v", err)
	}
	if el, ok := res.Result["element"].(*Node); !ok || el.Label != "General" {
		t.Fatalf("bad element: %+v", res.Result["element"])
	}
	if r.calls == 0 {
		t.Fatal("broken helper must fall back to a cold poll")
	}
}

func TestWarmActionErrorPropagates(t *testing.T) {
	ff := &fakeFactory{configure: func(h *fakeHelper) { h.errs["tap"] = "sim rebooted" }}
	cold := &coldFake{}
	b := warmFor(t, ff, cold)

	_, err := b.Dispatch(context.Background(), "U1",
		proto.ActionRequest{Kind: "tap", Payload: map[string]any{"x": 1, "y": 2}})
	if err == nil {
		t.Fatal("action error must propagate")
	}
	if len(cold.kinds) != 0 {
		t.Fatal("action errors must not be retried cold")
	}
	we := WireError(err)
	if we.Code != "internal" {
		t.Fatalf("wire code %q", we.Code)
	}
}

func TestHidOpUnknownOpIsColdOnly(t *testing.T) {
	ff := &fakeFactory{configure: func(h *fakeHelper) {
		h.errs["type"] = "unknown op type"
	}}
	b := warmFor(t, ff, &coldFake{})
	err := b.hidOp(context.Background(), "U1", "type", map[string]any{"text": "x"})
	if !errors.Is(err, errColdOnly) {
		t.Fatalf("err = %v, want errColdOnly", err)
	}
}
