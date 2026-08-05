package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

// kindFailBackend fails any action whose kind is in fail, echoing the rest.
type kindFailBackend struct {
	fail  map[string]bool
	kinds []string
}

func (b *kindFailBackend) Dispatch(_ context.Context, _ string, req proto.ActionRequest) (proto.ActionResult, error) {
	b.kinds = append(b.kinds, req.Kind)
	if b.fail[req.Kind] {
		return proto.ActionResult{}, errors.New("boom")
	}
	return proto.ActionResult{OK: true, Result: map[string]any{"echo": req.Kind}}, nil
}

func TestActionBatchDispatchesInOrder(t *testing.T) {
	backend := &kindFailBackend{}
	ts, _ := newJournaledActionServer(t, backend)
	l := acquire(t, ts)

	var res proto.BatchActionResult
	resp := doJSON(t, "POST", ts.URL+"/v0/actions:batch", proto.BatchActionRequest{
		LeaseID: l.ID,
		Actions: []proto.BatchAction{
			{Kind: "tap", Payload: map[string]any{"x": 1, "y": 2}},
			{Kind: "type", Payload: map[string]any{"text": "hi"}},
		},
	}, &res)
	if resp.StatusCode != 200 || !res.OK || res.Completed != 2 {
		t.Fatalf("batch: %d %+v", resp.StatusCode, res)
	}
	if len(res.Results) != 2 || !res.Results[0].OK || !res.Results[1].OK {
		t.Fatalf("results = %+v", res.Results)
	}
	if res.Results[1].Result["echo"] != "type" {
		t.Fatalf("second result = %+v, want echo=type", res.Results[1])
	}
	if len(backend.kinds) != 2 || backend.kinds[0] != "tap" || backend.kinds[1] != "type" {
		t.Fatalf("dispatch order = %v", backend.kinds)
	}
}

func TestActionBatchStopOnError(t *testing.T) {
	backend := &kindFailBackend{fail: map[string]bool{"type": true}}
	ts, _ := newJournaledActionServer(t, backend)
	l := acquire(t, ts)

	actions := []proto.BatchAction{
		{Kind: "tap", Payload: map[string]any{"x": 1, "y": 2}},
		{Kind: "type", Payload: map[string]any{"text": "hi"}},
		{Kind: "button", Payload: map[string]any{"name": "home"}},
	}

	var res proto.BatchActionResult
	resp := doJSON(t, "POST", ts.URL+"/v0/actions:batch", proto.BatchActionRequest{
		LeaseID: l.ID, StopOnError: true, Actions: actions,
	}, &res)
	if resp.StatusCode != 200 || res.OK {
		t.Fatalf("batch: %d %+v, want ok=false", resp.StatusCode, res)
	}
	if res.Completed != 2 || len(res.Results) != 2 {
		t.Fatalf("completed = %d results = %d, want 2 (halted)", res.Completed, len(res.Results))
	}
	if res.Results[1].OK || res.Results[1].Error == nil {
		t.Fatalf("second result = %+v, want error", res.Results[1])
	}

	// Without stop_on_error the remaining actions still run.
	backend.kinds = nil
	resp = doJSON(t, "POST", ts.URL+"/v0/actions:batch", proto.BatchActionRequest{
		LeaseID: l.ID, Actions: actions,
	}, &res)
	if resp.StatusCode != 200 || res.OK || res.Completed != 3 {
		t.Fatalf("batch: %d %+v, want 3 completed", resp.StatusCode, res)
	}
	if !res.Results[2].OK {
		t.Fatalf("third result = %+v, want ok", res.Results[2])
	}
}

func TestActionBatchRejectsEmptyAndOversized(t *testing.T) {
	ts, _ := newJournaledActionServer(t, &kindFailBackend{})
	l := acquire(t, ts)

	var res proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/actions:batch",
		proto.BatchActionRequest{LeaseID: l.ID}, &res)
	if resp.StatusCode != 400 {
		t.Fatalf("empty batch: %d, want 400", resp.StatusCode)
	}

	big := make([]proto.BatchAction, maxBatchActions+1)
	for i := range big {
		big[i] = proto.BatchAction{Kind: "tap"}
	}
	resp = doJSON(t, "POST", ts.URL+"/v0/actions:batch",
		proto.BatchActionRequest{LeaseID: l.ID, Actions: big}, &res)
	if resp.StatusCode != 400 {
		t.Fatalf("oversized batch: %d, want 400", resp.StatusCode)
	}
}

// A misnamed entry field (the classic "params" instead of "payload") must
// be rejected, not silently dropped and dispatched with defaults.
func TestActionBatchRejectsUnknownEntryKeys(t *testing.T) {
	backend := &kindFailBackend{}
	ts, _ := newJournaledActionServer(t, backend)
	l := acquire(t, ts)

	for _, body := range []string{
		`{"lease_id":%q,"actions":[{"kind":"tap","params":{"x":1,"y":2}}]}`,
		`{"lease_id":%q,"actions":[{"kind":"tap","payload":{"x":1,"y":2},"stop_on_error":true}]}`,
	} {
		var errRes proto.Error
		resp := doRaw(t, "POST", ts.URL+"/v0/actions:batch", fmt.Sprintf(body, l.ID), &errRes)
		if resp.StatusCode != 400 {
			t.Fatalf("body %s: %d, want 400", body, resp.StatusCode)
		}
		if errRes.Code != proto.ErrBadRequest || !strings.Contains(errRes.Message, "unknown field") {
			t.Fatalf("body %s: error = %+v, want a bad_request naming the unknown field", body, errRes)
		}
	}
	if len(backend.kinds) != 0 {
		t.Fatalf("no action should have been dispatched, got %v", backend.kinds)
	}

	// The valid shape still works.
	var ok proto.BatchActionResult
	resp := doRaw(t, "POST", ts.URL+"/v0/actions:batch",
		fmt.Sprintf(`{"lease_id":%q,"actions":[{"kind":"tap","payload":{"x":1,"y":2}}]}`, l.ID), &ok)
	if resp.StatusCode != 200 || !ok.OK {
		t.Fatalf("valid batch: %d %+v", resp.StatusCode, ok)
	}
}
