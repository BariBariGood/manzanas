package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// An explicit DELETE release must fire the lease-end hook so the pool can
// reclaim a daemon-booted sim (expiry reaches it via the event sink).
func TestReleaseFiresLeaseEndHook(t *testing.T) {
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	ended := make(chan string, 1)
	srv.SetLeaseEndHook(func(udid string) { ended <- udid })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var acquired proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"simulator"}, AgentID: "a1"}, &acquired)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("acquire: %d", resp.StatusCode)
	}
	resp = doJSON(t, "DELETE", ts.URL+"/v0/leases/"+acquired.ID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release: %d", resp.StatusCode)
	}
	select {
	case udid := <-ended:
		if udid != acquired.TargetUDID {
			t.Fatalf("hook udid = %q, want %q", udid, acquired.TargetUDID)
		}
	default:
		t.Fatal("release did not fire the lease-end hook")
	}
}

// The lease listing must carry the lease IDs: journal runs are keyed by
// them, and GET /v0/leases/{id} needs an ID to be usable from the list.
func TestListLeasesIncludesIDs(t *testing.T) {
	ts := newTestServer(t)

	var acquired proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"simulator"}, AgentID: "a1"}, &acquired)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("acquire: %d", resp.StatusCode)
	}
	if acquired.ID == "" {
		t.Fatal("acquire returned no lease ID")
	}

	var list struct {
		Leases []proto.Lease `json:"leases"`
	}
	resp = doJSON(t, "GET", ts.URL+"/v0/leases", nil, &list)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	if len(list.Leases) != 1 {
		t.Fatalf("want 1 lease, got %+v", list.Leases)
	}
	if list.Leases[0].ID != acquired.ID {
		t.Fatalf("list lease ID = %q, want %q", list.Leases[0].ID, acquired.ID)
	}
}
