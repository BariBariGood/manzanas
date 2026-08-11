package broker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrokerDashRoutes(t *testing.T) {
	d := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d.srv.URL}})
	ts := httptest.NewServer(b.Handler())
	t.Cleanup(ts.Close)

	for _, tc := range []struct {
		path  string
		ctype string
	}{
		{"/dash/", "text/html"},
		{"/dash/app.js", "text/javascript"},
		{"/dash/style.css", "text/css"}, // shared asset (internal/webui)
	} {
		resp, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("GET %s: status %d, want 200", tc.path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, tc.ctype) {
			t.Errorf("GET %s: content-type %q, want prefix %q", tc.path, ct, tc.ctype)
		}
	}

	// /dash redirects to /dash/ so relative asset URLs resolve.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(ts.URL + "/dash")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("GET /dash: status %d, want 301", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/dash/" {
		t.Errorf("GET /dash: location %q, want /dash/", loc)
	}

	resp, err = http.Get(ts.URL + "/dash/nope.js")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /dash/nope.js: status %d, want 404", resp.StatusCode)
	}
}

// TestDashDataSourcesWithDownHost exercises the three endpoints the
// broker dashboard polls, with one host down: the down host is reported
// in /v0/fleet/hosts (so the dash can mark it), while /v0/targets and
// /v0/leases keep serving the reachable host's data.
func TestDashDataSourcesWithDownHost(t *testing.T) {
	up := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	down := newFakeDaemon(t)
	downURL := down.srv.URL
	down.srv.Close()

	b := newTestBroker(t, []HostConfig{
		{Name: "alive", Addr: up.srv.URL},
		{Name: "dead", Addr: downURL},
	})
	h := b.Handler()

	rec, hosts := doJSON(t, h, "GET", "/v0/fleet/hosts", nil)
	if rec.Code != 200 {
		t.Fatalf("fleet hosts: %d", rec.Code)
	}
	list, _ := hosts["hosts"].([]any)
	if len(list) != 2 {
		t.Fatalf("hosts: %v", hosts)
	}
	byName := map[string]map[string]any{}
	for _, v := range list {
		m := v.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if up, _ := byName["alive"]["up"].(bool); !up {
		t.Errorf("alive host reported down: %v", byName["alive"])
	}
	if up, _ := byName["dead"]["up"].(bool); up {
		t.Errorf("dead host reported up: %v", byName["dead"])
	}
	if e, _ := byName["dead"]["last_error"].(string); e == "" {
		t.Errorf("dead host has no last_error")
	}

	rec, targets := doJSON(t, h, "GET", "/v0/targets", nil)
	if rec.Code != 200 {
		t.Fatalf("targets: %d", rec.Code)
	}
	ts, _ := targets["targets"].([]any)
	if len(ts) != 1 {
		t.Fatalf("targets: %v", targets)
	}
	if host := ts[0].(map[string]any)["host"]; host != "alive" {
		t.Errorf("target host = %v, want alive", host)
	}

	rec, leases := doJSON(t, h, "GET", "/v0/leases", nil)
	if rec.Code != 200 {
		t.Fatalf("leases: %d", rec.Code)
	}
	if ls, ok := leases["leases"].([]any); !ok || len(ls) != 0 {
		t.Errorf("leases: %v", leases)
	}
}
