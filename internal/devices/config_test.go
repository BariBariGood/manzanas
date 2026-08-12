package devices

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

func TestFromFlags(t *testing.T) {
	cfg, err := FromFlags(true,
		"UD1=http://127.0.0.1:8100,UD2=http://127.0.0.1:8200",
		"UD1=devicectl:com.manzanas.wda.xctrunner",
		"UD1=8100:8100",
		"UD3=mirror", "/tmp/mirrord.sock")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled {
		t.Error("enabled not carried over")
	}
	if got := cfg.WDA["UD1"]; got != (proto.DeviceWDAConfig{
		URL:     "http://127.0.0.1:8100",
		Launch:  "devicectl:com.manzanas.wda.xctrunner",
		Forward: "8100:8100",
	}) {
		t.Errorf("UD1 = %+v", got)
	}
	if got := cfg.WDA["UD2"]; got != (proto.DeviceWDAConfig{URL: "http://127.0.0.1:8200"}) {
		t.Errorf("UD2 = %+v", got)
	}
	if got := cfg.Mirror["UD3"]; got != (proto.DeviceMirrorConfig{Socket: "/tmp/mirrord.sock"}) {
		t.Errorf("UD3 = %+v", got)
	}
}

func TestParseBackends(t *testing.T) {
	m, err := ParseBackends("A=wda, B=mirror")
	if err != nil || m["A"] != "wda" || m["B"] != "mirror" || len(m) != 2 {
		t.Fatalf("m = %v, err = %v", m, err)
	}
	if _, err := ParseBackends(""); err != nil {
		t.Fatalf("empty spec: %v", err)
	}
	if _, err := ParseBackends("A=bogus"); err == nil {
		t.Fatal("bogus backend accepted")
	}
	if _, err := ParseBackends("A"); err == nil {
		t.Fatal("missing '=' accepted")
	}
	// Harmless repeats deduplicate; conflicting redefinitions error.
	m, err = ParseBackends("A=mirror,A=mirror")
	if err != nil || m["A"] != "mirror" || len(m) != 1 {
		t.Fatalf("duplicate mirror entries: m = %v, err = %v", m, err)
	}
	if _, err := ParseBackends("A=mirror,A=wda"); err == nil {
		t.Fatal("conflicting duplicate accepted")
	}
}

func TestFromFlagsErrors(t *testing.T) {
	cases := []struct{ wda, launch, forward, want string }{
		{"UD1", "", "", "invalid --device-wda"},
		{"", "UD1=devicectl:x", "", "without a wda url"},
		{"UD1=http://x", "UD1=bogus", "", "invalid WDA launch spec"},
		{"UD1=http://x", "", "UD1=notports", "invalid forward spec"},
		{"UD1=http://x", "", "UD1=0:8100", "invalid local port"},
	}
	for _, c := range cases {
		_, err := FromFlags(true, c.wda, c.launch, c.forward, "", "")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("FromFlags(%q,%q,%q) err = %v, want %q", c.wda, c.launch, c.forward, err, c.want)
		}
	}
	// The mirror is one global window per Mac: two mirror devices are
	// rejected, as is a device that is both mirror- and WDA-backed.
	if _, err := FromFlags(true, "", "", "", "A=mirror,B=mirror", ""); err == nil ||
		!strings.Contains(err.Error(), "at most one mirror-backed device") {
		t.Errorf("two mirror devices: err = %v", err)
	}
	if _, err := FromFlags(true, "A=http://x", "", "", "A=mirror", ""); err == nil ||
		!strings.Contains(err.Error(), "pick one backend per device") {
		t.Errorf("mirror+wda device: err = %v", err)
	}
}

func TestValidateRejectsCrossDeviceCollisions(t *testing.T) {
	cases := []struct {
		cfg  proto.DevicesConfig
		want string
	}{
		{proto.DevicesConfig{Enabled: true, WDA: map[string]proto.DeviceWDAConfig{
			"UD1": {URL: "http://127.0.0.1:8100", Forward: "8100:8100"},
			"UD2": {URL: "http://127.0.0.1:8101", Forward: "8100:8100"},
		}}, "duplicate forward local port"},
		{proto.DevicesConfig{Enabled: true, WDA: map[string]proto.DeviceWDAConfig{
			"UD1": {URL: "http://127.0.0.1:8100"},
			"UD2": {URL: "http://127.0.0.1:8100"},
		}}, "duplicate wda url"},
	}
	for _, c := range cases {
		err := Validate(c.cfg)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Validate err = %v, want %q", err, c.want)
		}
	}
}

func TestLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	want := proto.DevicesConfig{
		Enabled: true,
		WDA: map[string]proto.DeviceWDAConfig{
			"UD1": {URL: "http://127.0.0.1:8100", Launch: "xctestrun:/tmp/w.xctestrun", Forward: "8100:8100"},
		},
	}
	if err := WriteFile(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled != want.Enabled || got.WDA["UD1"] != want.WDA["UD1"] {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, []byte(`{"enabled":true,"wda":{},"typo":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "typo") {
		t.Errorf("Load err = %v, want unknown-field error", err)
	}
}

func TestLoadRejectsInvalidSpecs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, []byte(`{"enabled":true,"wda":{"UD1":{"url":"http://x","launch":"nope"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "launch spec") {
		t.Errorf("Load err = %v, want launch spec error", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); !os.IsNotExist(err) {
		t.Errorf("Load err = %v, want not-exist", err)
	}
}
