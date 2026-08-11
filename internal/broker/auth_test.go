package broker

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/internal/server"
	"github.com/BariBariGood/manzanas/proto"
)

// newAuthedFakeDaemon is newFakeDaemon with --auth-token set.
func newAuthedFakeDaemon(t *testing.T, token string, targets ...proto.Target) *httptest.Server {
	t.Helper()
	reg := registry.NewMock(targets...)
	s := server.New(reg, nil, slog.Default())
	m := lease.New(reg, s.EventSink())
	t.Cleanup(m.Close)
	s.SetLeases(m)
	s.SetAuthToken(token)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestBrokerAuthToken(t *testing.T) {
	const token = "sekrit"
	d := newFakeDaemon(t, sim("SIM-1", "iOS 18.2", "iPhone 16"))
	b := New(Config{Hosts: []HostConfig{{Name: "h1", Addr: d.srv.URL}}},
		slog.Default(), Options{ProbeInterval: time.Hour, AuthToken: token})
	b.probeAll(context.Background())
	ts := httptest.NewServer(b.Handler())
	t.Cleanup(ts.Close)

	get := func(path string, mod func(*http.Request)) int {
		req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if mod != nil {
			mod(req)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := get("/v0/healthz", nil); got != http.StatusOK {
		t.Errorf("GET /v0/healthz without token: status %d, want 200", got)
	}
	if got := get("/v0/fleet/hosts", nil); got != http.StatusUnauthorized {
		t.Errorf("GET /v0/fleet/hosts without token: status %d, want 401", got)
	}
	if got := get("/v0/fleet/hosts", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer nope")
	}); got != http.StatusUnauthorized {
		t.Errorf("GET /v0/fleet/hosts wrong token: status %d, want 401", got)
	}
	if got := get("/v0/fleet/hosts", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
	}); got != http.StatusOK {
		t.Errorf("GET /v0/fleet/hosts right token: status %d, want 200", got)
	}
	if got := get("/v0/fleet/hosts?token="+token, nil); got != http.StatusOK {
		t.Errorf("GET /v0/fleet/hosts?token=: status %d, want 200", got)
	}
	if got := get("/dash/", nil); got != http.StatusOK {
		t.Errorf("GET /dash/ without token: status %d, want 200", got)
	}
}

// The broker forwards per-host tokens to fronted daemons; a daemon running
// with --auth-token accepts probes and lease traffic only when the broker
// is configured with its token.
func TestBrokerDaemonTokenForwarding(t *testing.T) {
	const token = "daemon-sekrit"
	ts := newAuthedFakeDaemon(t, token, sim("SIM-1", "iOS 18.2", "iPhone 16"))

	// Without the token the probe's target listing fails.
	b := newTestBroker(t, []HostConfig{{Name: "h1", Addr: ts.URL}})
	if h := b.hosts[0].health(); h.Targets != 0 {
		t.Fatalf("without token: health %+v, want 0 targets", h)
	}

	// With the per-host token the probe sees the daemon's targets.
	b = newTestBroker(t, []HostConfig{{Name: "h1", Addr: ts.URL, Token: token}})
	if h := b.hosts[0].health(); !h.Up || h.Targets != 1 {
		t.Fatalf("with token: health %+v, want up with 1 target", h)
	}
}

// Options.DaemonToken is the fleet-wide default; a per-host token overrides.
func TestBrokerDefaultDaemonToken(t *testing.T) {
	const token = "fleet-sekrit"
	ts := newAuthedFakeDaemon(t, token, sim("SIM-1", "iOS 18.2", "iPhone 16"))

	b := New(Config{Hosts: []HostConfig{{Name: "h1", Addr: ts.URL}}},
		slog.Default(), Options{ProbeInterval: time.Hour, DaemonToken: token})
	b.probeAll(context.Background())
	if h := b.hosts[0].health(); !h.Up || h.Targets != 1 {
		t.Fatalf("with default daemon token: health %+v, want up with 1 target", h)
	}
}
