package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
)

func newAuthTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	srv.SetAuthToken(token)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, url string, mod func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
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
	return resp
}

func TestAuthDisabledByDefault(t *testing.T) {
	ts := newAuthTestServer(t, "")
	for _, path := range []string{"/v0/healthz", "/v0/targets", "/v0/leases"} {
		if resp := get(t, ts.URL+path, nil); resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s without token: status %d, want 200", path, resp.StatusCode)
		}
	}
}

func TestAuthToken(t *testing.T) {
	const token = "sekrit"
	ts := newAuthTestServer(t, token)

	// Health stays credential-free (probes, load balancers).
	if resp := get(t, ts.URL+"/v0/healthz", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v0/healthz without token: status %d, want 200", resp.StatusCode)
	}

	// Everything else requires the token.
	cases := []struct {
		name string
		mod  func(*http.Request)
		want int
	}{
		{"no token", nil, http.StatusUnauthorized},
		{"wrong token", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer nope")
		}, http.StatusUnauthorized},
		{"right token", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
		}, http.StatusOK},
	}
	for _, c := range cases {
		if resp := get(t, ts.URL+"/v0/targets", c.mod); resp.StatusCode != c.want {
			t.Errorf("GET /v0/targets (%s): status %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}

	// Query-parameter token for browser contexts that cannot set headers.
	if resp := get(t, ts.URL+"/v0/targets?token="+token, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v0/targets?token=: status %d, want 200", resp.StatusCode)
	}
	if resp := get(t, ts.URL+"/v0/targets?token=nope", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /v0/targets?token=nope: status %d, want 401", resp.StatusCode)
	}

	// Mutations are gated too.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v0/leases",
		strings.NewReader(`{"agent_id":"a","ttl_seconds":30}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /v0/leases without token: status %d, want 401", resp.StatusCode)
	}

	// The dashboard shell stays reachable (its JS prompts for the token).
	if resp := get(t, ts.URL+"/dash/", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /dash/ without token: status %d, want 200", resp.StatusCode)
	}
}

// With a token configured, stream CORS echoes the request origin (never *)
// so only token-holding pages can read responses.
func TestAuthStreamsCORSEchoesOrigin(t *testing.T) {
	ts := newAuthTestServer(t, "sekrit")
	req, err := http.NewRequest(http.MethodOptions, ts.URL+"/v0/streams", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://broker.example:7440")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS /v0/streams: status %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://broker.example:7440" {
		t.Errorf("Access-Control-Allow-Origin %q, want echoed origin", got)
	}

	// The 401 for a missing/wrong token carries the CORS headers too, so
	// the broker dash can read the failure and prompt for the token.
	req, err = http.NewRequest(http.MethodPost, ts.URL+"/v0/streams", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://broker.example:7440")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /v0/streams without token: status %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://broker.example:7440" {
		t.Errorf("401 Access-Control-Allow-Origin %q, want echoed origin", got)
	}
}
