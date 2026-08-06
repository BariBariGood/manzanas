package server

import (
	"context"
	"encoding/base64"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/actions"
	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// recordingBackend captures the UDID and request the server dispatched.
type recordingBackend struct {
	udid   string
	req    proto.ActionRequest
	err    error
	result map[string]any // returned instead of the default echo when set
}

func (b *recordingBackend) Dispatch(_ context.Context, udid string, req proto.ActionRequest) (proto.ActionResult, error) {
	b.udid, b.req = udid, req
	if b.err != nil {
		return proto.ActionResult{}, b.err
	}
	if b.result != nil {
		return proto.ActionResult{OK: true, Result: b.result}, nil
	}
	return proto.ActionResult{OK: true, Result: map[string]any{"echo": req.Kind}}, nil
}

// newJournaledActionServer wires a journal store alongside the backend so
// tests can assert what dispatchAction records.
func newJournaledActionServer(t *testing.T, backend actions.Backend) (*httptest.Server, journal.Store) {
	t.Helper()
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	store, err := journal.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv.SetJournal(journal.NewRecorder(store))
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	srv.SetActions(backend)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store
}

func TestActionJournalsAXHashes(t *testing.T) {
	backend := &recordingBackend{result: map[string]any{"ax_before": "sha256:aaa", "ax_after": "sha256:bbb"}}
	ts, store := newJournaledActionServer(t, backend)
	l := acquire(t, ts)

	var res proto.ActionResult
	resp := doJSON(t, "POST", ts.URL+"/v0/actions",
		proto.ActionRequest{LeaseID: l.ID, Kind: "tap", Payload: map[string]any{"x": 1, "y": 2, "ax_hashes": true}}, &res)
	if resp.StatusCode != 200 || !res.OK {
		t.Fatalf("dispatch: %d %+v", resp.StatusCode, res)
	}
	e := lastEntryOfKind(t, store, l.ID, "action")
	if e.Payload["ax_before"] != "sha256:aaa" || e.Payload["ax_after"] != "sha256:bbb" {
		t.Fatalf("entry payload = %+v, want ax_before/ax_after", e.Payload)
	}

	// An observe's computed tree hash lands as ax_after.
	backend.result = map[string]any{"hash": "sha256:ccc"}
	resp = doJSON(t, "POST", ts.URL+"/v0/actions",
		proto.ActionRequest{LeaseID: l.ID, Kind: "observe"}, &res)
	if resp.StatusCode != 200 {
		t.Fatalf("observe: %d", resp.StatusCode)
	}
	e = lastEntryOfKind(t, store, l.ID, "action")
	if e.Payload["ax_after"] != "sha256:ccc" {
		t.Fatalf("observe entry payload = %+v, want ax_after=sha256:ccc", e.Payload)
	}
}

func TestScreenshotJournaledAsArtifact(t *testing.T) {
	png := "\x89PNG\r\n\x1a\nfake"
	backend := &recordingBackend{result: map[string]any{
		"format": "png", "png_base64": base64.StdEncoding.EncodeToString([]byte(png)),
	}}
	ts, store := newJournaledActionServer(t, backend)
	l := acquire(t, ts)

	var res proto.ActionResult
	resp := doJSON(t, "POST", ts.URL+"/v0/actions",
		proto.ActionRequest{LeaseID: l.ID, Kind: "screenshot", Payload: map[string]any{"inline": false}}, &res)
	if resp.StatusCode != 200 || !res.OK {
		t.Fatalf("dispatch: %d %+v", resp.StatusCode, res)
	}
	// inline:false strips the wire payload but the pixels live on as a
	// content-addressed journal artifact.
	if _, ok := res.Result["png_base64"]; ok {
		t.Fatal("inline=false response should omit png_base64")
	}
	e := lastEntryOfKind(t, store, l.ID, "action")
	arts, _ := e.Payload["artifacts"].([]any)
	if len(arts) != 1 {
		t.Fatalf("entry artifacts = %+v, want one ref", e.Payload["artifacts"])
	}
	ref, _ := arts[0].(map[string]any)
	rc, err := store.OpenArtifact(l.ID, ref["path"].(string))
	if err != nil {
		t.Fatalf("open artifact: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != png {
		t.Fatalf("artifact round-trip mismatch: %q", got)
	}
}

// lastEntryOfKind returns the newest journal entry of the given kind.
func lastEntryOfKind(t *testing.T, store journal.Store, runID, kind string) journal.Entry {
	t.Helper()
	entries, err := store.Read(context.Background(), runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind == kind {
			return entries[i]
		}
	}
	t.Fatalf("no %q entry in %d entries", kind, len(entries))
	return journal.Entry{}
}

func newActionServer(t *testing.T, backend actions.Backend) (*httptest.Server, *lease.Manager) {
	t.Helper()
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	if backend != nil {
		srv.SetActions(backend)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, leases
}

func acquire(t *testing.T, ts *httptest.Server) proto.Lease {
	t.Helper()
	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "test"}, &l)
	if resp.StatusCode != 201 || l.State != proto.LeaseActive {
		t.Fatalf("acquire: %d %+v", resp.StatusCode, l)
	}
	return l
}

func TestActionDispatchWithLease(t *testing.T) {
	backend := &recordingBackend{}
	ts, _ := newActionServer(t, backend)
	l := acquire(t, ts)

	var res proto.ActionResult
	resp := doJSON(t, "POST", ts.URL+"/v0/actions",
		proto.ActionRequest{LeaseID: l.ID, Kind: "tap", Payload: map[string]any{"x": 1, "y": 2}}, &res)
	if resp.StatusCode != 200 || !res.OK {
		t.Fatalf("dispatch: %d %+v", resp.StatusCode, res)
	}
	if backend.udid != l.TargetUDID {
		t.Fatalf("backend got udid %q, want %q", backend.udid, l.TargetUDID)
	}
	if backend.req.Kind != "tap" {
		t.Fatalf("backend got kind %q", backend.req.Kind)
	}
}

func TestActionRequiresValidLease(t *testing.T) {
	tests := []struct {
		name    string
		leaseID func(l proto.Lease) string
		release bool
		status  int
		code    string
	}{
		{
			name:    "unknown lease",
			leaseID: func(proto.Lease) string { return "lse_nope" },
			status:  404, code: proto.ErrNotFound,
		},
		{
			name:    "released lease",
			leaseID: func(l proto.Lease) string { return l.ID },
			release: true,
			status:  410, code: proto.ErrLeaseExpired,
		},
		{
			name:    "missing lease id",
			leaseID: func(proto.Lease) string { return "" },
			status:  404, code: proto.ErrNotFound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend := &recordingBackend{}
			ts, _ := newActionServer(t, backend)
			l := acquire(t, ts)
			if tc.release {
				doJSON(t, "DELETE", ts.URL+"/v0/leases/"+l.ID, nil, nil)
			}
			var e proto.Error
			resp := doJSON(t, "POST", ts.URL+"/v0/actions",
				proto.ActionRequest{LeaseID: tc.leaseID(l), Kind: "tap"}, &e)
			if resp.StatusCode != tc.status || e.Code != tc.code {
				t.Fatalf("got %d %q, want %d %q", resp.StatusCode, e.Code, tc.status, tc.code)
			}
			if backend.udid != "" {
				t.Fatal("backend was invoked without a valid lease")
			}
		})
	}
}

func TestActionWithoutBackendIsNotImplemented(t *testing.T) {
	ts, _ := newActionServer(t, nil)
	l := acquire(t, ts)
	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/actions", proto.ActionRequest{LeaseID: l.ID, Kind: "tap"}, &e)
	if resp.StatusCode != 501 || e.Code != proto.ErrNotImplemented {
		t.Fatalf("got %d %q", resp.StatusCode, e.Code)
	}
}

func TestActionBackendErrorMapping(t *testing.T) {
	tests := []struct {
		code   string
		status int
	}{
		{proto.ErrBadRequest, 400},
		{proto.ErrUnavailable, 503},
		{proto.ErrNotImplemented, 501},
		{proto.ErrInternal, 500},
	}
	for _, tc := range tests {
		t.Run(tc.code, func(t *testing.T) {
			backend := &recordingBackend{err: &actions.Error{Code: tc.code, Message: "boom"}}
			ts, _ := newActionServer(t, backend)
			l := acquire(t, ts)
			var e proto.Error
			resp := doJSON(t, "POST", ts.URL+"/v0/actions",
				proto.ActionRequest{LeaseID: l.ID, Kind: "tap"}, &e)
			if resp.StatusCode != tc.status || e.Code != tc.code {
				t.Fatalf("got %d %q, want %d %q", resp.StatusCode, e.Code, tc.status, tc.code)
			}
		})
	}
}

func TestActionRequiresKind(t *testing.T) {
	ts, _ := newActionServer(t, &recordingBackend{})
	l := acquire(t, ts)
	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/actions", proto.ActionRequest{LeaseID: l.ID}, &e)
	if resp.StatusCode != 400 || e.Code != proto.ErrBadRequest {
		t.Fatalf("got %d %q", resp.StatusCode, e.Code)
	}
	// The message must hint at the request envelope so a caller who sent
	// e.g. {"type": "tap"} can self-correct without reading the source.
	for _, want := range []string{"lease_id", "kind", "payload"} {
		if !strings.Contains(e.Message, want) {
			t.Fatalf("kind-required message %q must mention %q", e.Message, want)
		}
	}
}
