package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

// fakeDaemon is a minimal daemon: it answers lease-scoped routes and
// counts the requests it served.
type fakeDaemon struct {
	srv      *httptest.Server
	requests atomic.Int64
	leases   map[string]proto.Lease // by ID; returned un-annotated
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{leases: map[string]proto.Lease{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/actions", func(w http.ResponseWriter, r *http.Request) {
		d.requests.Add(1)
		var req proto.ActionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if _, ok := d.leases[req.LeaseID]; !ok {
			writeErr(w, http.StatusNotFound, proto.ErrNotFound, "lease not found")
			return
		}
		writeOK(w, proto.ActionResult{OK: true, Result: map[string]any{"served_by": d.srv.URL}})
	})
	mux.HandleFunc("GET /v0/leases/{id}", func(w http.ResponseWriter, r *http.Request) {
		d.requests.Add(1)
		l, ok := d.leases[r.PathValue("id")]
		if !ok {
			writeErr(w, http.StatusNotFound, proto.ErrNotFound, "lease not found")
			return
		}
		writeOK(w, l)
	})
	mux.HandleFunc("DELETE /v0/leases/{id}", func(w http.ResponseWriter, r *http.Request) {
		d.requests.Add(1)
		l, ok := d.leases[r.PathValue("id")]
		if !ok {
			writeErr(w, http.StatusNotFound, proto.ErrNotFound, "lease not found")
			return
		}
		l.State = proto.LeaseReleased
		d.leases[l.ID] = l
		writeOK(w, l)
	})
	mux.HandleFunc("GET /v0/journal/{id}", func(w http.ResponseWriter, r *http.Request) {
		d.requests.Add(1)
		if _, ok := d.leases[r.PathValue("id")]; !ok {
			writeErr(w, http.StatusNotFound, proto.ErrNotFound, "unknown run")
			return
		}
		writeOK(w, map[string]any{"entries": []any{}, "next_seq": 0})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		d.requests.Add(1)
		writeErr(w, http.StatusNotFound, proto.ErrNotFound, "unknown route")
	})
	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

// fakeBroker serves only placement routes (targets, leases, fleet) like
// manzanas-broker, annotating leases with host/host_addr, and answers
// everything else with the enveloped 404 {"not_found","unknown route"}.
type fakeBroker struct {
	srv    *httptest.Server
	daemon *fakeDaemon
	name   string
	// forgetful drops lease lookups (simulating a restarted broker that
	// lost its lease routing table).
	forgetful bool
}

func newFakeBroker(t *testing.T, d *fakeDaemon) *fakeBroker {
	t.Helper()
	b := &fakeBroker{daemon: d, name: "mac1"}
	mux := http.NewServeMux()
	annotate := func(l proto.Lease) proto.Lease {
		l.Host = b.name
		l.HostAddr = d.srv.URL
		return l
	}
	mux.HandleFunc("POST /v0/leases", func(w http.ResponseWriter, r *http.Request) {
		l := proto.Lease{ID: "lse_test", State: proto.LeaseActive, AgentID: "t"}
		d.leases[l.ID] = l
		writeJSONStatus(w, http.StatusCreated, annotate(l))
	})
	mux.HandleFunc("GET /v0/leases/{id}", func(w http.ResponseWriter, r *http.Request) {
		l, ok := b.daemon.leases[r.PathValue("id")]
		if !ok || b.forgetful {
			writeErr(w, http.StatusNotFound, proto.ErrNotFound, "lease not known to this broker")
			return
		}
		writeOK(w, annotate(l))
	})
	mux.HandleFunc("DELETE /v0/leases/{id}", func(w http.ResponseWriter, r *http.Request) {
		l, ok := b.daemon.leases[r.PathValue("id")]
		if !ok || b.forgetful {
			writeErr(w, http.StatusNotFound, proto.ErrNotFound, "lease not known to this broker")
			return
		}
		l.State = proto.LeaseReleased
		b.daemon.leases[l.ID] = l
		writeOK(w, annotate(l))
	})
	mux.HandleFunc("GET /v0/fleet/hosts", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]any{"hosts": []map[string]any{
			{"name": b.name, "addr": d.srv.URL, "up": true, "targets": 1},
		}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, proto.ErrNotFound, "unknown route")
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

func writeOK(w http.ResponseWriter, v any) { writeJSONStatus(w, http.StatusOK, v) }
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSONStatus(w, status, proto.Error{Code: code, Message: msg})
}
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestBrokerAcquireThenDispatchRoutesToDaemon(t *testing.T) {
	d := newFakeDaemon(t)
	b := newFakeBroker(t, d)
	c := New(b.srv.URL)
	ctx := context.Background()

	l, err := c.AcquireLease(ctx, proto.AcquireLeaseRequest{AgentID: "t"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if l.HostAddr != d.srv.URL {
		t.Fatalf("host_addr = %q, want %q", l.HostAddr, d.srv.URL)
	}
	res, err := c.Dispatch(ctx, proto.ActionRequest{LeaseID: l.ID, Kind: "observe"})
	if err != nil {
		t.Fatalf("dispatch through broker: %v", err)
	}
	if got := res.Result["served_by"]; got != d.srv.URL {
		t.Fatalf("served_by = %v, want %q", got, d.srv.URL)
	}
	if got := c.AddrForLease(l.ID); got != d.srv.URL {
		t.Fatalf("AddrForLease = %q, want %q", got, d.srv.URL)
	}
}

func TestFreshClientResolvesRouteViaLeaseLookup(t *testing.T) {
	d := newFakeDaemon(t)
	b := newFakeBroker(t, d)
	d.leases["lse_prior"] = proto.Lease{ID: "lse_prior", State: proto.LeaseActive}

	// A fresh client (new CLI process) that never saw the acquire.
	c := New(b.srv.URL)
	res, err := c.Dispatch(context.Background(), proto.ActionRequest{LeaseID: "lse_prior", Kind: "observe"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := res.Result["served_by"]; got != d.srv.URL {
		t.Fatalf("served_by = %v, want %q", got, d.srv.URL)
	}
}

func TestReleaseFallsBackToDaemonWhenBrokerDown(t *testing.T) {
	d := newFakeDaemon(t)
	b := newFakeBroker(t, d)
	c := New(b.srv.URL)
	ctx := context.Background()

	l, err := c.AcquireLease(ctx, proto.AcquireLeaseRequest{AgentID: "t"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	b.srv.Close()
	rel, err := c.ReleaseLease(ctx, l.ID)
	if err != nil {
		t.Fatalf("release with broker down: %v", err)
	}
	if rel.State != proto.LeaseReleased {
		t.Fatalf("state = %q, want released", rel.State)
	}
}

func TestJournalReadOfFinishedRunFansOutOverFleetHosts(t *testing.T) {
	d := newFakeDaemon(t)
	b := newFakeBroker(t, d)
	d.leases["lse_done"] = proto.Lease{ID: "lse_done", State: proto.LeaseReleased}
	b.forgetful = true // broker restarted: lease routing table is gone

	c := New(b.srv.URL)
	if _, _, err := c.JournalRead(context.Background(), "lse_done", 0, 100); err != nil {
		t.Fatalf("journal read via fan-out: %v", err)
	}
	// The answering host was cached as the run's route, so follow-up
	// reads (and AddrForLease) go there directly.
	if got := c.AddrForLease("lse_done"); got != d.srv.URL {
		t.Fatalf("AddrForLease after fan-out = %q, want %q", got, d.srv.URL)
	}
}

func TestFanOutSkipsBrokenHostAndFindsOwner(t *testing.T) {
	d := newFakeDaemon(t)
	d.leases["lse_old"] = proto.Lease{ID: "lse_old", State: proto.LeaseReleased}
	// A daemon that cannot serve the journal at all (e.g. built without
	// the journal slice): answers 501 for everything.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotImplemented, proto.ErrNotImplemented, "no journal store")
	}))
	defer broken.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/fleet/hosts", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]any{"hosts": []map[string]any{
			{"name": "broken", "addr": broken.URL, "up": true},
			{"name": "owner", "addr": d.srv.URL, "up": true},
		}})
	})
	mux.HandleFunc("GET /v0/leases/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, proto.ErrNotFound, "lease not known to this broker")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, proto.ErrNotFound, "unknown route")
	})
	b := httptest.NewServer(mux)
	defer b.Close()

	c := New(b.URL)
	if _, _, err := c.JournalRead(context.Background(), "lse_old", 0, 100); err != nil {
		t.Fatalf("journal read should reach the owner past the broken host: %v", err)
	}
}

func TestFanOutNeverSpraysMutatingRequests(t *testing.T) {
	d := newFakeDaemon(t)
	b := newFakeBroker(t, d)
	d.leases["lse_gone"] = proto.Lease{ID: "lse_gone", State: proto.LeaseActive}
	b.forgetful = true

	c := New(b.srv.URL)
	before := d.requests.Load()
	_, err := c.Dispatch(context.Background(), proto.ActionRequest{LeaseID: "lse_gone", Kind: "tap"})
	if err == nil {
		t.Fatal("want the broker's route-miss error for an unresolvable POST")
	}
	if got := d.requests.Load() - before; got != 0 {
		t.Fatalf("daemon received %d requests; a POST must not fan out", got)
	}
}

func TestSingleDaemonPathUnchanged(t *testing.T) {
	d := newFakeDaemon(t)
	d.leases["lse_x"] = proto.Lease{ID: "lse_x", State: proto.LeaseActive}
	c := New(d.srv.URL)

	before := d.requests.Load()
	if _, err := c.Dispatch(context.Background(), proto.ActionRequest{LeaseID: "lse_x", Kind: "observe"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := d.requests.Load() - before; got != 1 {
		t.Fatalf("daemon served %d requests, want exactly 1 (no routing overhead)", got)
	}
}

func TestGenuineNotFoundIsNotRetried(t *testing.T) {
	d := newFakeDaemon(t)
	c := New(d.srv.URL)
	_, err := c.Dispatch(context.Background(), proto.ActionRequest{LeaseID: "lse_missing", Kind: "observe"})
	if err == nil || !strings.Contains(err.Error(), "lease not found") {
		t.Fatalf("err = %v, want the daemon's lease-not-found error", err)
	}
}

func TestNormalizeAddr(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:7433":         "http://127.0.0.1:7433",
		"http://mac1:7433/":      "http://mac1:7433",
		"https://mac1.tail:7433": "https://mac1.tail:7433",
	} {
		if got := normalizeAddr(in); got != want {
			t.Fatalf("normalizeAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
