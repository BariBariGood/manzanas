package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func doJSON(t *testing.T, method, url string, body any, out any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp
}

// doRaw posts a literal JSON body (doJSON re-encodes a Go value, which
// can't express malformed or unknown wire fields).
func doRaw(t *testing.T, method, url, body string, out any) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp
}

func TestRESTLeaseLifecycle(t *testing.T) {
	ts := newTestServer(t)

	var listResp struct {
		Targets []proto.Target `json:"targets"`
	}
	resp := doJSON(t, "GET", ts.URL+"/v0/targets", nil, &listResp)
	if resp.StatusCode != 200 || len(listResp.Targets) == 0 {
		t.Fatalf("targets: %d %+v", resp.StatusCode, listResp)
	}

	var l proto.Lease
	resp = doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "test"}, &l)
	if resp.StatusCode != 201 || l.State != proto.LeaseActive {
		t.Fatalf("acquire: %d %+v", resp.StatusCode, l)
	}

	var booted proto.Target
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/boot",
		map[string]string{"lease_id": l.ID}, &booted)
	if resp.StatusCode != 202 || booted.State != proto.StateBooted {
		t.Fatalf("boot: %d %+v", resp.StatusCode, booted)
	}

	var renewed proto.Lease
	resp = doJSON(t, "POST", ts.URL+"/v0/leases/"+l.ID+"/renew",
		proto.RenewLeaseRequest{TTLSeconds: 600}, &renewed)
	if resp.StatusCode != 200 || renewed.TTLSeconds != 600 {
		t.Fatalf("renew: %d %+v", resp.StatusCode, renewed)
	}

	var shut proto.Target
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/shutdown",
		map[string]string{"lease_id": l.ID}, &shut)
	if resp.StatusCode != 202 || shut.State != proto.StateShutdown {
		t.Fatalf("shutdown: %d %+v", resp.StatusCode, shut)
	}

	var renewedDefault proto.Lease
	resp = doJSON(t, "POST", ts.URL+"/v0/leases/"+l.ID+"/renew", nil, &renewedDefault)
	if resp.StatusCode != 200 || renewedDefault.TTLSeconds != 600 {
		t.Fatalf("renew empty body: %d %+v", resp.StatusCode, renewedDefault)
	}

	var released proto.Lease
	resp = doJSON(t, "DELETE", ts.URL+"/v0/leases/"+l.ID, nil, &released)
	if resp.StatusCode != 200 || released.State != proto.LeaseReleased {
		t.Fatalf("release: %d %+v", resp.StatusCode, released)
	}
}

func TestRESTErrors(t *testing.T) {
	ts := newTestServer(t)

	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios99"}, AgentID: "test"}, &e)
	if resp.StatusCode != 409 || e.Code != proto.ErrNoMatch {
		t.Fatalf("no_match: %d %+v", resp.StatusCode, e)
	}

	resp = doJSON(t, "GET", ts.URL+"/v0/leases/lse_nope", nil, &e)
	if resp.StatusCode != 404 || e.Code != proto.ErrNotFound {
		t.Fatalf("not_found: %d %+v", resp.StatusCode, e)
	}

	resp = doJSON(t, "POST", ts.URL+"/v0/actions",
		proto.ActionRequest{LeaseID: "x", Kind: "tap"}, &e)
	if resp.StatusCode != 501 || e.Code != proto.ErrNotImplemented {
		t.Fatalf("actions stub: %d %+v", resp.StatusCode, e)
	}

	resp = doJSON(t, "POST", ts.URL+"/v0/streams",
		proto.StreamRequest{LeaseID: "x", Format: "mjpeg"}, &e)
	if resp.StatusCode != 501 {
		t.Fatalf("streams stub: %d", resp.StatusCode)
	}
}

func TestBootRequiresLease(t *testing.T) {
	ts := newTestServer(t)
	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/targets/MOCK-0000-0000-0001/boot",
		map[string]string{"lease_id": "lse_nope"}, &e)
	if resp.StatusCode != 404 {
		t.Fatalf("boot without lease: %d %+v", resp.StatusCode, e)
	}
}

func TestWSLifecycle(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v0/ws"
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	call := func(id, method string, params any) proto.Envelope {
		t.Helper()
		raw, _ := json.Marshal(params)
		if err := wsjson.Write(ctx, conn, proto.Envelope{V: proto.Version, ID: id, Method: method, Params: raw}); err != nil {
			t.Fatal(err)
		}
		for {
			var resp proto.Envelope
			if err := wsjson.Read(ctx, conn, &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Event != "" {
				continue // skip broadcast events
			}
			if resp.ID != id {
				t.Fatalf("response id = %q, want %q", resp.ID, id)
			}
			return resp
		}
	}

	resp := call("1", proto.MethodListTargets, nil)
	if resp.Error != nil {
		t.Fatalf("targets.list: %+v", resp.Error)
	}

	resp = call("2", proto.MethodAcquireLease, proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "ws-test"})
	if resp.Error != nil {
		t.Fatalf("leases.acquire: %+v", resp.Error)
	}
	var l proto.Lease
	if err := json.Unmarshal(resp.Result, &l); err != nil {
		t.Fatal(err)
	}
	if l.State != proto.LeaseActive {
		t.Fatalf("lease = %+v", l)
	}

	resp = call("3", proto.MethodBootTarget, map[string]string{"udid": l.TargetUDID, "lease_id": l.ID})
	if resp.Error != nil {
		t.Fatalf("targets.boot: %+v", resp.Error)
	}

	resp = call("4", proto.MethodAction, proto.ActionRequest{LeaseID: l.ID, Kind: "tap"})
	if resp.Error == nil || resp.Error.Code != proto.ErrNotImplemented {
		t.Fatalf("actions.dispatch: %+v", resp.Error)
	}

	resp = call("5", proto.MethodReleaseLease, map[string]string{"id": l.ID})
	if resp.Error != nil {
		t.Fatalf("leases.release: %+v", resp.Error)
	}
}
