package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// fakeDevMgr records Apply calls and serves a canned current config.
type fakeDevMgr struct {
	cur     proto.DevicesConfig
	applied []proto.DevicesConfig
	err     error
}

func (f *fakeDevMgr) Apply(cfg proto.DevicesConfig) error {
	if f.err != nil {
		return f.err
	}
	f.applied = append(f.applied, cfg)
	f.cur = cfg
	return nil
}

func (f *fakeDevMgr) Current() proto.DevicesConfig { return f.cur }

func newDevicesAdminServer(t *testing.T, mgr DevicesManager) *httptest.Server {
	t.Helper()
	srv := New(registry.NewMock(), nil, nil)
	// POST /v0/devices makes the daemon spawn host processes, so it
	// refuses to serve on an unauthenticated daemon.
	srv.SetAuthToken("tok")
	if mgr != nil {
		srv.SetDevicesManager(mgr)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestDevicesRoutesNotImplementedWithoutManager(t *testing.T) {
	ts := newDevicesAdminServer(t, nil)
	var e proto.Error
	if resp := doJSON(t, "GET", ts.URL+"/v0/devices?token=tok", nil, &e); resp.StatusCode != http.StatusNotImplemented || e.Code != proto.ErrNotImplemented {
		t.Fatalf("GET got %d %+v", resp.StatusCode, e)
	}
	if resp := doJSON(t, "POST", ts.URL+"/v0/devices?token=tok", proto.DevicesConfig{}, &e); resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("POST got %d %+v", resp.StatusCode, e)
	}
}

func TestDevicesGetReturnsCurrent(t *testing.T) {
	mgr := &fakeDevMgr{cur: proto.DevicesConfig{Enabled: true,
		WDA: map[string]proto.DeviceWDAConfig{"UD1": {URL: "http://127.0.0.1:8100"}}}}
	ts := newDevicesAdminServer(t, mgr)
	var got proto.DevicesConfig
	if resp := doJSON(t, "GET", ts.URL+"/v0/devices?token=tok", nil, &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if !got.Enabled || got.WDA["UD1"].URL != "http://127.0.0.1:8100" {
		t.Errorf("got %+v", got)
	}
}

func TestDevicesApply(t *testing.T) {
	mgr := &fakeDevMgr{}
	ts := newDevicesAdminServer(t, mgr)
	cfg := proto.DevicesConfig{Enabled: true,
		WDA: map[string]proto.DeviceWDAConfig{"UD1": {URL: "http://127.0.0.1:8100", Forward: "8100:8100"}}}
	var got proto.DevicesConfig
	if resp := doJSON(t, "POST", ts.URL+"/v0/devices?token=tok", cfg, &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if len(mgr.applied) != 1 || mgr.applied[0].WDA["UD1"].Forward != "8100:8100" {
		t.Errorf("applied = %+v", mgr.applied)
	}
	if !got.Enabled {
		t.Errorf("response = %+v", got)
	}
}

func TestDevicesApplyRejectsBadConfig(t *testing.T) {
	mgr := &fakeDevMgr{err: errBadConfig{}}
	ts := newDevicesAdminServer(t, mgr)
	var e proto.Error
	if resp := doJSON(t, "POST", ts.URL+"/v0/devices?token=tok", proto.DevicesConfig{}, &e); resp.StatusCode != http.StatusBadRequest || e.Code != proto.ErrBadRequest {
		t.Fatalf("got %d %+v", resp.StatusCode, e)
	}
	if !strings.Contains(e.Message, "bad devices config") {
		t.Errorf("message = %q", e.Message)
	}
}

func TestDevicesApplyRejectsEmptyBody(t *testing.T) {
	mgr := &fakeDevMgr{cur: proto.DevicesConfig{Enabled: true}}
	ts := newDevicesAdminServer(t, mgr)
	resp, err := http.Post(ts.URL+"/v0/devices?token=tok", "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if len(mgr.applied) != 0 {
		t.Errorf("empty body must not reach Apply: %+v", mgr.applied)
	}
}

func TestDevicesApplyRequiresAuthToken(t *testing.T) {
	mgr := &fakeDevMgr{}
	srv := New(registry.NewMock(), nil, nil)
	srv.SetDevicesManager(mgr)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	var e proto.Error
	if resp := doJSON(t, "POST", ts.URL+"/v0/devices", proto.DevicesConfig{Enabled: true}, &e); resp.StatusCode != http.StatusForbidden || e.Code != proto.ErrUnauthorized {
		t.Fatalf("got %d %+v", resp.StatusCode, e)
	}
	if len(mgr.applied) != 0 {
		t.Errorf("unauthenticated apply must not reach the manager: %+v", mgr.applied)
	}
}

func TestDevicesApplyRejectsUnknownFields(t *testing.T) {
	mgr := &fakeDevMgr{cur: proto.DevicesConfig{Enabled: true}}
	ts := newDevicesAdminServer(t, mgr)
	resp, err := http.Post(ts.URL+"/v0/devices?token=tok", "application/json",
		strings.NewReader(`{"enabled":true,"wdaa":{"UD1":{"url":"http://127.0.0.1:8100"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if len(mgr.applied) != 0 {
		t.Errorf("typoed config must not reach Apply: %+v", mgr.applied)
	}
}

type errBadConfig struct{}

func (errBadConfig) Error() string { return "bad devices config" }
