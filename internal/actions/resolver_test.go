package actions

import (
	"context"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

// The simulator ladder: tap_element resolves through the AX rung and the
// result carries the rung's provenance.
func TestResolverLadderSimAXProvenance(t *testing.T) {
	b, _ := compositeBackend(seqStep{stdout: treeJSON("Settings", "General")})
	res, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "General"}))
	if err != nil {
		t.Fatalf("tap_element: %v", err)
	}
	if res["resolver"] != "ax" {
		t.Fatalf("resolver = %v, want ax", res["resolver"])
	}
}

// The mirror ladder: OCR is the only registered rung and provenance says
// so.
func TestResolverLadderMirrorOCRProvenance(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Sign In", "x": 100.0, "y": 200.0, "w": 120.0, "h": 40.0},
	}
	res, err := deviceDispatch(t, b, "tap_element", fastWait(map[string]any{"label": "Sign In"}))
	if err != nil {
		t.Fatalf("tap_element: %v", err)
	}
	if res.Result["resolver"] != "ocr" {
		t.Fatalf("resolver = %v, want ocr", res.Result["resolver"])
	}
}

// WDA-only devices register no OCR rung: with the runner dead the ladder
// has nothing else to try, and repeated fast rung-1 failures open the
// per-bundle circuit, which then stays open across subsequent steps.
func TestResolverLadderWDACircuitOpensAndStaysOpen(t *testing.T) {
	f := &fakeWDAServer{sourceXML: sampleWDAXML, down: true}
	b := testDeviceBackendWithWDA(t, f)
	ctx := WithLeaseID(context.Background(), "lse_test")
	payload := map[string]any{"label": "Log In", "timeout_ms": 250.0, "interval_ms": 10.0}
	dispatch := func() error {
		_, err := b.Dispatch(ctx, "DEV-UDID", proto.ActionRequest{Kind: "tap_element", Payload: payload})
		return err
	}
	// Fail steps until the breaker trips (threshold consecutive fast
	// rung-1 failures for the same lease+bundle).
	for i := 0; i < axBreakerThreshold; i++ {
		if err := dispatch(); err == nil {
			t.Fatalf("step %d: want error with WDA down", i)
		}
	}
	key := breakerKey(ctx, "DEV-UDID", "")
	if !b.axBreak.isOpen(key) {
		t.Fatal("circuit should be open after threshold fast failures")
	}
	// The next step skips the AX rung entirely and escalates with the
	// skip journaled in the bundle; the circuit stays open.
	err := dispatch()
	if err == nil {
		t.Fatal("want escalation with circuit open")
	}
	bundle, _ := EscalationEvidence(err)
	if bundle == nil {
		t.Fatalf("want escalation bundle, got %v", err)
	}
	if bundle["resolver_skipped"] != "ax(circuit-open)" {
		t.Fatalf("resolver_skipped = %v, want ax(circuit-open)", bundle["resolver_skipped"])
	}
	if !b.axBreak.isOpen(key) {
		t.Fatal("circuit must stay open across steps")
	}
	// A fresh lease re-arms the probe: its key is closed.
	if b.axBreak.isOpen(breakerKey(WithLeaseID(context.Background(), "lse_other"), "DEV-UDID", "")) {
		t.Fatal("another lease's circuit must not be open")
	}
}

// The escalation bundle shape: rung trail, compact text boxes from the
// last observation, and the wire error still a timeout carrying the
// off-screen-aware message.
func TestResolverLadderEscalationBundleShape(t *testing.T) {
	b, _ := seqBackend(seqStep{stdout: treeJSON("Settings", "General")})
	_, err := handleTapElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "Missing"}))
	if err == nil {
		t.Fatal("want escalation error")
	}
	ae, ok := err.(*Error)
	if !ok || ae.Code != "timeout" {
		t.Fatalf("err = %v, want *Error with timeout code", err)
	}
	if !strings.Contains(ae.Message, "did not resolve") || !strings.Contains(ae.Message, "ax=no-match") {
		t.Fatalf("message = %q, want rung trail", ae.Message)
	}
	bundle, _ := EscalationEvidence(err)
	if bundle == nil {
		t.Fatal("want escalation bundle")
	}
	if _, ok := bundle["rungs"].(string); !ok {
		t.Fatalf("bundle rungs = %v, want string trail", bundle["rungs"])
	}
	boxes, ok := bundle["text_boxes"].([]map[string]any)
	if !ok || len(boxes) != 2 {
		t.Fatalf("text_boxes = %v, want 2 boxes", bundle["text_boxes"])
	}
	if boxes[0]["text"] != "Settings" || boxes[1]["text"] != "General" {
		t.Fatalf("boxes = %v", boxes)
	}
	for _, k := range []string{"x", "y", "w", "h"} {
		if _, ok := boxes[0][k].(float64); !ok {
			t.Fatalf("box missing %q: %v", k, boxes[0])
		}
	}
}

// Success resets the breaker's consecutive count; an open circuit stays
// open.
func TestAXBreakerCounting(t *testing.T) {
	br := newAXBreaker()
	key := "lease\x00bundle"
	for i := 0; i < axBreakerThreshold-1; i++ {
		if br.fail(key) {
			t.Fatalf("opened after %d failures, threshold is %d", i+1, axBreakerThreshold)
		}
	}
	br.succeed(key)
	if br.fail(key) {
		t.Fatal("success must reset the consecutive-failure count")
	}
	for i := 0; i < axBreakerThreshold-1; i++ {
		br.fail(key)
	}
	if !br.isOpen(key) {
		t.Fatal("circuit should open at threshold")
	}
	br.succeed(key)
	if !br.isOpen(key) {
		t.Fatal("an open circuit stays open for the lease")
	}
}
