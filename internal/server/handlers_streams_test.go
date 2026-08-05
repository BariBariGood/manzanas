package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/internal/stream"
	"github.com/BariBariGood/manzanas/proto"
)

func newStreamTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	reg := registry.NewMock()
	// Streaming requires booted targets.
	targets, err := reg.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tgt := range targets {
		if err := reg.Boot(context.Background(), tgt.UDID); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(reg, nil, nil)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	streamer := stream.NewManager(stream.Config{}, stream.FakeSourceFactory, nil)
	t.Cleanup(streamer.CloseAll)
	srv.SetStreamer(streamer)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestOpenStreamByUDIDWithoutLease(t *testing.T) {
	ts := newStreamTestServer(t)

	var offer proto.StreamOffer
	resp := doJSON(t, http.MethodPost, ts.URL+"/v0/streams",
		proto.StreamRequest{UDID: "MOCK-0000-0000-0001"}, &offer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if offer.Format != "mjpeg" || offer.StreamID == "" {
		t.Fatalf("bad offer: %+v", offer)
	}
	if offer.Holder != nil {
		t.Errorf("holder = %+v, want nil (target is free)", offer.Holder)
	}
}

func TestOpenStreamIncludesLeaseHolder(t *testing.T) {
	ts := newStreamTestServer(t)

	var l proto.Lease
	doJSON(t, http.MethodPost, ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "agent-a"}, &l)
	if l.State != proto.LeaseActive {
		t.Fatalf("lease state = %s", l.State)
	}

	var offer proto.StreamOffer
	resp := doJSON(t, http.MethodPost, ts.URL+"/v0/streams",
		proto.StreamRequest{UDID: l.TargetUDID}, &offer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if offer.Holder == nil || offer.Holder.AgentID != "agent-a" {
		t.Fatalf("holder = %+v, want agent-a", offer.Holder)
	}
	if offer.Holder.ID != "" {
		t.Errorf("holder.ID = %q, want redacted (capability token) for a UDID-addressed request", offer.Holder.ID)
	}
}

func TestOpenStreamByLeaseID(t *testing.T) {
	ts := newStreamTestServer(t)

	var l proto.Lease
	doJSON(t, http.MethodPost, ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "agent-a"}, &l)

	var offer proto.StreamOffer
	resp := doJSON(t, http.MethodPost, ts.URL+"/v0/streams",
		proto.StreamRequest{LeaseID: l.ID}, &offer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if offer.ViewURL != "/view/"+l.TargetUDID {
		t.Errorf("view_url = %q", offer.ViewURL)
	}
	if offer.Holder == nil || offer.Holder.ID != l.ID {
		t.Errorf("holder = %+v, want ID echoed back to the lease holder", offer.Holder)
	}
}

func TestOpenStreamValidation(t *testing.T) {
	ts := newStreamTestServer(t)

	var e proto.Error
	resp := doJSON(t, http.MethodPost, ts.URL+"/v0/streams", proto.StreamRequest{}, &e)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty request status = %d, want 400", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodPost, ts.URL+"/v0/streams",
		proto.StreamRequest{UDID: "NOPE"}, &e)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown target status = %d, want 404", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodPost, ts.URL+"/v0/streams",
		proto.StreamRequest{UDID: "MOCK-0000-0000-0001", Format: "h264"}, &e)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("h264 status = %d, want 400", resp.StatusCode)
	}
}

func TestOpenStreamRejectsShutdownTarget(t *testing.T) {
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	streamer := stream.NewManager(stream.Config{}, stream.FakeSourceFactory, nil)
	t.Cleanup(streamer.CloseAll)
	srv.SetStreamer(streamer)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var e proto.Error
	resp := doJSON(t, http.MethodPost, ts.URL+"/v0/streams",
		proto.StreamRequest{UDID: "MOCK-0000-0000-0001"}, &e)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	if e.Code != proto.ErrTargetBusy {
		t.Errorf("code = %q, want %q", e.Code, proto.ErrTargetBusy)
	}
}

func TestViewPageServed(t *testing.T) {
	ts := newStreamTestServer(t)
	resp, err := http.Get(ts.URL + "/view/MOCK-0000-0000-0001")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestStreamsNotImplementedWithoutStreamer(t *testing.T) {
	ts := newTestServer(t)
	var e proto.Error
	resp := doJSON(t, http.MethodPost, ts.URL+"/v0/streams",
		proto.StreamRequest{UDID: "MOCK-0000-0000-0001"}, &e)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}
