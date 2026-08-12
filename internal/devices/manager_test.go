package devices

import (
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/actions"
	"github.com/BariBariGood/manzanas/proto"
)

func testManager() (*Manager, *bool) {
	enabled := false
	backend := actions.NewDevice()
	m := NewManager(backend, func(on bool, _ []string) { enabled = on }, nil)
	return m, &enabled
}

func TestManagerApplyTogglesEnumeration(t *testing.T) {
	m, enabled := testManager()
	defer m.Close()
	if err := m.Apply(proto.DevicesConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if !*enabled {
		t.Error("enable toggle not called")
	}
	if err := m.Apply(proto.DevicesConfig{}); err != nil {
		t.Fatal(err)
	}
	if *enabled {
		t.Error("disable toggle not called")
	}
}

func TestManagerApplyAddChangeRemove(t *testing.T) {
	m, _ := testManager()
	defer m.Close()
	if err := m.Apply(proto.DevicesConfig{Enabled: true, WDA: map[string]proto.DeviceWDAConfig{
		"UD1": {URL: "http://127.0.0.1:8100"},
	}}); err != nil {
		t.Fatal(err)
	}
	if got := m.Current(); got.WDA["UD1"].URL != "http://127.0.0.1:8100" {
		t.Fatalf("Current = %+v", got)
	}
	if err := m.Apply(proto.DevicesConfig{Enabled: true, WDA: map[string]proto.DeviceWDAConfig{
		"UD1": {URL: "http://127.0.0.1:8101"},
		"UD2": {URL: "http://127.0.0.1:8200"},
	}}); err != nil {
		t.Fatal(err)
	}
	got := m.Current()
	if got.WDA["UD1"].URL != "http://127.0.0.1:8101" || got.WDA["UD2"].URL != "http://127.0.0.1:8200" {
		t.Fatalf("Current after change = %+v", got)
	}
	if err := m.Apply(proto.DevicesConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := m.Current(); len(got.WDA) != 0 {
		t.Fatalf("Current after removal = %+v", got)
	}
}

func TestManagerApplyValidationLeavesStateUntouched(t *testing.T) {
	m, enabled := testManager()
	defer m.Close()
	if err := m.Apply(proto.DevicesConfig{Enabled: true, WDA: map[string]proto.DeviceWDAConfig{
		"UD1": {URL: "http://127.0.0.1:8100"},
	}}); err != nil {
		t.Fatal(err)
	}
	err := m.Apply(proto.DevicesConfig{WDA: map[string]proto.DeviceWDAConfig{
		"UD2": {Launch: "devicectl:x"},
	}})
	if err == nil || !strings.Contains(err.Error(), "without a wda url") {
		t.Fatalf("Apply err = %v, want validation error", err)
	}
	if got := m.Current(); !got.Enabled || got.WDA["UD1"].URL != "http://127.0.0.1:8100" {
		t.Errorf("state changed on failed apply: %+v", got)
	}
	if !*enabled {
		t.Error("enable toggle flipped on failed apply")
	}
}

func TestManagerDisabledConfigIsInert(t *testing.T) {
	m, enabled := testManager()
	defer m.Close()
	// enabled=false: config remembered but nothing wired or supervised
	// (the old --devices semantics: WDA flags without --devices are inert).
	if err := m.Apply(proto.DevicesConfig{WDA: map[string]proto.DeviceWDAConfig{
		"UD1": {URL: "http://127.0.0.1:8100"},
	}}); err != nil {
		t.Fatal(err)
	}
	if *enabled {
		t.Error("enabled flipped on")
	}
	if got := m.Current(); got.WDA["UD1"].URL != "http://127.0.0.1:8100" {
		t.Errorf("Current lost the disabled config: %+v", got)
	}
	if len(m.sups) != 0 || len(m.wired) != 0 {
		t.Errorf("disabled config started supervisors/wiring: sups=%d wired=%d", len(m.sups), len(m.wired))
	}
	// Re-enabling wires everything up.
	if err := m.Apply(proto.DevicesConfig{Enabled: true, WDA: map[string]proto.DeviceWDAConfig{
		"UD1": {URL: "http://127.0.0.1:8100"},
	}}); err != nil {
		t.Fatal(err)
	}
	_, wired := m.wired["UD1"]
	if !*enabled || !wired {
		t.Errorf("re-enable did not wire: enabled=%t wired=%v", *enabled, m.wired)
	}
}

func TestManagerAppliesAndRemovesMirror(t *testing.T) {
	m, _ := testManager()
	defer m.Close()
	if err := m.Apply(proto.DevicesConfig{Enabled: true, Mirror: map[string]proto.DeviceMirrorConfig{
		"UD1": {Socket: "/tmp/m.sock"},
	}}); err != nil {
		t.Fatal(err)
	}
	if m.wiredMirror["UD1"] != "/tmp/m.sock" || len(m.sups) != 0 {
		t.Fatalf("mirror not wired (or supervisor started): wiredMirror=%v sups=%d", m.wiredMirror, len(m.sups))
	}
	if got := m.Current().Mirror["UD1"]; got != (proto.DeviceMirrorConfig{Socket: "/tmp/m.sock"}) {
		t.Errorf("Current lost mirror config: %+v", got)
	}
	// Removing the device (and disabling) detaches the mirror client.
	if err := m.Apply(proto.DevicesConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if len(m.wiredMirror) != 0 {
		t.Errorf("mirror still wired after removal: %v", m.wiredMirror)
	}
	if err := m.Apply(proto.DevicesConfig{Enabled: false, Mirror: map[string]proto.DeviceMirrorConfig{
		"UD1": {Socket: "/tmp/m.sock"},
	}}); err != nil {
		t.Fatal(err)
	}
	if len(m.wiredMirror) != 0 {
		t.Errorf("disabled config wired a mirror client: %v", m.wiredMirror)
	}
}

func TestManagerRejectsWDAAndMirrorSameDevice(t *testing.T) {
	m, _ := testManager()
	defer m.Close()
	err := m.Apply(proto.DevicesConfig{Enabled: true,
		WDA:    map[string]proto.DeviceWDAConfig{"UD1": {URL: "http://127.0.0.1:8100"}},
		Mirror: map[string]proto.DeviceMirrorConfig{"UD1": {}},
	})
	if err == nil || !strings.Contains(err.Error(), "pick one backend per device") {
		t.Errorf("err = %v", err)
	}
}

func TestManagerCurrentIsACopy(t *testing.T) {
	m, _ := testManager()
	defer m.Close()
	if err := m.Apply(proto.DevicesConfig{Enabled: true, WDA: map[string]proto.DeviceWDAConfig{
		"UD1": {URL: "http://127.0.0.1:8100"},
	}}); err != nil {
		t.Fatal(err)
	}
	got := m.Current()
	got.WDA["UD1"] = proto.DeviceWDAConfig{URL: "mutated"}
	if m.Current().WDA["UD1"].URL != "http://127.0.0.1:8100" {
		t.Error("Current returned a shared map")
	}
}
