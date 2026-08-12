package main

import (
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

func TestMergeDevicesPreservesMirror(t *testing.T) {
	base := proto.DevicesConfig{Enabled: true,
		WDA:    map[string]proto.DeviceWDAConfig{"UD1": {URL: "http://127.0.0.1:8100"}},
		Mirror: map[string]proto.DeviceMirrorConfig{"UD2": {Socket: "/tmp/m.sock"}},
	}
	add := proto.DevicesConfig{Enabled: true,
		WDA: map[string]proto.DeviceWDAConfig{"UD3": {URL: "http://127.0.0.1:8101"}},
	}
	out := mergeDevices(base, add)
	if out.Mirror["UD2"] != (proto.DeviceMirrorConfig{Socket: "/tmp/m.sock"}) {
		t.Errorf("mirror entry lost: %+v", out.Mirror)
	}
	if out.WDA["UD1"].URL != "http://127.0.0.1:8100" || out.WDA["UD3"].URL != "http://127.0.0.1:8101" {
		t.Errorf("wda merge wrong: %+v", out.WDA)
	}
	if got := mergeDevices(proto.DevicesConfig{Enabled: true}, add); got.Mirror != nil {
		t.Errorf("mirror map fabricated: %+v", got.Mirror)
	}
}

func TestMergeDevicesReplacesBackendPerDevice(t *testing.T) {
	// Re-onboarding a mirror-backed phone as WDA must drop its mirror
	// entry (Validate rejects a UDID present in both maps) — and vice
	// versa.
	base := proto.DevicesConfig{Enabled: true,
		Mirror: map[string]proto.DeviceMirrorConfig{"UD1": {Socket: "/tmp/m.sock"}},
	}
	out := mergeDevices(base, proto.DevicesConfig{Enabled: true,
		WDA: map[string]proto.DeviceWDAConfig{"UD1": {URL: "http://127.0.0.1:8100"}},
	})
	if out.Mirror != nil || out.WDA["UD1"].URL != "http://127.0.0.1:8100" {
		t.Errorf("mirror->wda replace: %+v", out)
	}
	base = proto.DevicesConfig{Enabled: true,
		WDA: map[string]proto.DeviceWDAConfig{"UD1": {URL: "http://127.0.0.1:8100"}},
	}
	out = mergeDevices(base, proto.DevicesConfig{Enabled: true,
		Mirror: map[string]proto.DeviceMirrorConfig{"UD1": {}},
	})
	if len(out.WDA) != 0 || len(out.Mirror) != 1 {
		t.Errorf("wda->mirror replace: %+v", out)
	}
}
