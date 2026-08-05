package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

func TestDashRoutes(t *testing.T) {
	ts := newTestServer(t)

	for _, tc := range []struct {
		path  string
		ctype string
	}{
		{"/dash/", "text/html"},
		{"/dash/app.js", "text/javascript"},
		{"/dash/style.css", "text/css"},
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

func TestListTargetsWarm(t *testing.T) {
	ts := newTestServer(t)

	var list struct {
		Targets []proto.Target `json:"targets"`
	}
	resp := doJSON(t, "GET", ts.URL+"/v0/targets", nil, &list)
	if resp.StatusCode != 200 || len(list.Targets) == 0 {
		t.Fatalf("targets: %d %+v", resp.StatusCode, list)
	}
	// No pool wired: warm must be absent/false everywhere.
	for _, tgt := range list.Targets {
		if tgt.Warm {
			t.Errorf("target %s warm without a pool", tgt.UDID)
		}
	}
}

func TestListTargetsWarmWithPool(t *testing.T) {
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	all, err := reg.List(context.Background())
	if err != nil || len(all) == 0 {
		t.Fatalf("mock registry list: %v %d", err, len(all))
	}
	parkedUDID := all[0].UDID
	srv.SetParkedCheck(func(udid string) bool { return udid == parkedUDID })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var list struct {
		Targets []proto.Target `json:"targets"`
	}
	resp := doJSON(t, "GET", ts.URL+"/v0/targets", nil, &list)
	if resp.StatusCode != 200 {
		t.Fatalf("targets: %d", resp.StatusCode)
	}
	found := false
	for _, tgt := range list.Targets {
		if tgt.UDID == parkedUDID {
			found = true
			if !tgt.Warm {
				t.Errorf("target %s: warm=false, want true", tgt.UDID)
			}
		} else if tgt.Warm {
			t.Errorf("target %s: warm=true, want false", tgt.UDID)
		}
	}
	if !found {
		t.Fatalf("parked target %s not listed", parkedUDID)
	}
}
