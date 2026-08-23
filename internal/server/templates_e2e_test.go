package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/BariBariGood/manzanas/internal/actions/mockapp"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/internal/templates"
	"github.com/BariBariGood/manzanas/proto"
)

// TestTemplatesEndToEndOverHTTP drives the storage half of the template
// library exactly as `manzanasd --mock --templates-dir ...` wires it:
// lease → boot → save_template → GET /v0/templates → DELETE, verifying
// the PNG + sidecar land on (and leave) disk.
func TestTemplatesEndToEndOverHTTP(t *testing.T) {
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	tplDir := t.TempDir()
	tpl, err := templates.NewStore(tplDir)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetTemplates(tpl)
	store := mockapp.NewStore()
	backend := mockapp.NewBackend(store, mockapp.WithBooted(func(ctx context.Context, udid string) bool {
		st, err := reg.Health(ctx, udid)
		return err == nil && st == proto.StateBooted
	}))
	backend.SetTemplateStore(tpl)
	srv.SetActions(backend)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "tpl-e2e"}, &l)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("acquire: %d", resp.StatusCode)
	}
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/boot",
		map[string]any{"lease_id": l.ID}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("boot: %d", resp.StatusCode)
	}

	var res proto.ActionResult
	resp = doJSON(t, "POST", ts.URL+"/v0/actions", proto.ActionRequest{
		LeaseID: l.ID, Kind: "save_template", Payload: map[string]any{
			"name":      "login-button",
			"bundle_id": "com.example.mock",
			"rect":      map[string]any{"x": 10, "y": 20, "w": 40, "h": 16},
		}}, &res)
	if resp.StatusCode != http.StatusOK || !res.OK {
		t.Fatalf("save_template: %d %+v", resp.StatusCode, res)
	}
	class, _ := res.Result["device_class"].(string)
	if class == "" {
		t.Fatalf("save_template result missing device_class: %+v", res.Result)
	}

	var list struct {
		Templates []templates.Entry `json:"templates"`
	}
	resp = doJSON(t, "GET", ts.URL+"/v0/templates", nil, &list)
	if resp.StatusCode != http.StatusOK || len(list.Templates) != 1 {
		t.Fatalf("list: %d %+v", resp.StatusCode, list)
	}
	e := list.Templates[0]
	if e.BundleID != "com.example.mock" || e.Name != "login-button" ||
		e.DeviceClass != class || e.CropRect.W != 40 || e.CropRect.H != 16 {
		t.Fatalf("bad entry: %+v", e)
	}
	bundleDir := filepath.Join(tplDir, "com.example.mock")
	files, err := os.ReadDir(bundleDir)
	if err != nil || len(files) != 2 {
		t.Fatalf("on-disk files: %v %v", files, err)
	}

	var del struct {
		Removed int `json:"removed"`
	}
	resp = doJSON(t, "DELETE", ts.URL+"/v0/templates/com.example.mock/login-button", nil, &del)
	if resp.StatusCode != http.StatusOK || del.Removed != 1 {
		t.Fatalf("delete: %d %+v", resp.StatusCode, del)
	}
	if _, err := os.Stat(bundleDir); !os.IsNotExist(err) {
		t.Fatalf("bundle dir survived delete: %v", err)
	}
	resp = doJSON(t, "DELETE", ts.URL+"/v0/templates/com.example.mock/login-button", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete: %d, want 404", resp.StatusCode)
	}
}
