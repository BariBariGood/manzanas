package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// TestTargetsIncludeMirrorDevices exercises the full /v0/targets path
// over a merged registry with a mirror source: a mirror-backed phone that
// the device registry also knows (metadata overlay) and one it does not
// (synthesized entry) both enumerate with mirror-derived liveness.
func TestTargetsIncludeMirrorDevices(t *testing.T) {
	devices := registry.NewMock(proto.Target{
		UDID: "PAIRED-PHONE", Kind: proto.TargetDevice, Name: "test-iphone",
		Runtime: "iOS 26.5", DeviceType: "iPhone 15 Pro", State: proto.StateUnknown,
		Labels: []string{"device", "ios26", "ios26.5", "iphone-15-pro", registry.DisconnectedLabel},
	})
	mirrors := registry.NewMirror(func(ctx context.Context, socket string) (mirror.Status, error) {
		if socket == "/tmp/phone.sock" {
			return mirror.Status{State: "ready", Frontmost: true}, nil
		}
		return mirror.Status{}, errors.New("mirrord unreachable")
	})
	mirrors.SetMirrors(map[string]string{
		"PAIRED-PHONE":     "/tmp/phone.sock",
		"MIRROR-ONLY-UDID": "/tmp/only.sock",
	})
	srv := New(registry.Merge(devices, mirrors), nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var out struct {
		Targets []proto.Target `json:"targets"`
	}
	resp := doJSON(t, http.MethodGet, ts.URL+"/v0/targets", nil, &out)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v0/targets = %d", resp.StatusCode)
	}
	if len(out.Targets) != 2 {
		t.Fatalf("got %d targets, want 2 (dedup by UDID): %+v", len(out.Targets), out.Targets)
	}
	byUDID := map[string]proto.Target{}
	for _, tg := range out.Targets {
		byUDID[tg.UDID] = tg
	}

	phone := byUDID["PAIRED-PHONE"]
	if phone.Name != "test-iphone" || phone.State != proto.StateBooted {
		t.Fatalf("overlaid phone = %+v", phone)
	}
	for _, want := range []string{registry.MirrorLabel, registry.NoTunnelLabel, "iphone-15-pro"} {
		if !slices.Contains(phone.Labels, want) {
			t.Fatalf("phone labels missing %q: %v", want, phone.Labels)
		}
	}
	if slices.Contains(phone.Labels, registry.NoDevicectlLabel) || slices.Contains(phone.Labels, registry.DisconnectedLabel) {
		t.Fatalf("phone labels = %v", phone.Labels)
	}

	only := byUDID["MIRROR-ONLY-UDID"]
	if only.Kind != proto.TargetDevice || only.Name != "iPhone (mirror)" || only.State != proto.StateUnknown {
		t.Fatalf("synthesized target = %+v", only)
	}
	for _, want := range []string{registry.MirrorLabel, registry.NoDevicectlLabel, registry.DisconnectedLabel} {
		if !slices.Contains(only.Labels, want) {
			t.Fatalf("synthesized labels missing %q: %v", want, only.Labels)
		}
	}
}
