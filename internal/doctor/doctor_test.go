package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
	"github.com/BariBariGood/manzanas/proto"
)

// find returns the named check or fails the test.
func find(t *testing.T, r proto.DoctorReport, name string) proto.DoctorCheck {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q missing from report: %+v", name, r.Checks)
	return proto.DoctorCheck{}
}

// TestReportMockHost is the Linux/--mock shape: sim/devicectl checks say
// "not applicable" (never fail), and with nothing configured the report
// is green.
func TestReportMockHost(t *testing.T) {
	e := Env{Build: "test", Protocol: "v0", SimTooling: false, JournalDir: t.TempDir()}
	r := e.Report(context.Background(), "")
	if !r.OK {
		t.Fatalf("mock host report not OK: %+v", r.Checks)
	}
	for _, name := range []string{"xcrun", "ios_runtime", "devicectl"} {
		if c := find(t, r, name); c.Status != proto.DoctorNA {
			t.Fatalf("%s = %q, want n/a", name, c.Status)
		}
	}
	if c := find(t, r, "boot_gates"); c.Status != proto.DoctorNA {
		t.Fatalf("boot_gates = %q, want n/a (no pool)", c.Status)
	}
	if c := find(t, r, "devices"); c.Status != proto.DoctorNA {
		t.Fatalf("devices = %q, want n/a (no device manager)", c.Status)
	}
	if c := find(t, r, "journal"); c.Status != proto.DoctorOK {
		t.Fatalf("journal = %q, want ok", c.Status)
	}
}

// TestReportSimToolchain drives the darwin-shaped checks with fake
// tool runners.
func TestReportSimToolchain(t *testing.T) {
	runtimesJSON := []byte(`{"runtimes":[
		{"name":"iOS 26.5","isAvailable":true},
		{"name":"iOS 17.0","isAvailable":false},
		{"name":"watchOS 26.5","isAvailable":true}]}`)
	e := Env{
		Build: "test", Protocol: "v0", SimTooling: true, JournalDir: t.TempDir(),
		LookPath: func(string) (string, error) { return "/usr/bin/xcrun", nil },
		RunOut: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if args[0] == "simctl" {
				return runtimesJSON, nil
			}
			return []byte("devicectl version 397"), nil
		},
	}
	r := e.Report(context.Background(), "")
	if !r.OK {
		t.Fatalf("report not OK: %+v", r.Checks)
	}
	if c := find(t, r, "ios_runtime"); c.Detail != "iOS 26.5" {
		t.Fatalf("ios_runtime detail = %q (available iOS runtimes only)", c.Detail)
	}
	if c := find(t, r, "devicectl"); c.Status != proto.DoctorOK {
		t.Fatalf("devicectl = %q, want ok", c.Status)
	}

	// No available iOS runtime: fail with the download remedy.
	e.RunOut = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if args[0] == "simctl" {
			return []byte(`{"runtimes":[{"name":"iOS 17.0","isAvailable":false}]}`), nil
		}
		return nil, errors.New("no devicectl")
	}
	r = e.Report(context.Background(), "")
	if r.OK {
		t.Fatal("report OK despite missing iOS runtime")
	}
	c := find(t, r, "ios_runtime")
	if c.Status != proto.DoctorFail || !strings.Contains(c.Remedy, "downloadPlatform") {
		t.Fatalf("ios_runtime = %+v, want fail with download remedy", c)
	}
	// devicectl missing is a warn (simulators still work), not a fail.
	if c := find(t, r, "devicectl"); c.Status != proto.DoctorWarn {
		t.Fatalf("devicectl = %q, want warn", c.Status)
	}

	// xcrun missing: everything toolchain-shaped fails with remedies.
	e.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	r = e.Report(context.Background(), "")
	for _, name := range []string{"xcrun", "ios_runtime", "devicectl"} {
		if c := find(t, r, name); c.Status != proto.DoctorFail || c.Remedy == "" {
			t.Fatalf("%s = %+v, want fail with a remedy", name, c)
		}
	}
}

// TestReportGates maps pool gate states onto the boot_gates check.
func TestReportGates(t *testing.T) {
	e := Env{Build: "test", Protocol: "v0", JournalDir: t.TempDir()}
	e.Gates = func(context.Context) (bool, bool, bool) { return true, true, true }
	if c := find(t, e.Report(context.Background(), ""), "boot_gates"); c.Status != proto.DoctorOK {
		t.Fatalf("open gates = %q, want ok", c.Status)
	}
	e.Gates = func(context.Context) (bool, bool, bool) { return false, true, true }
	c := find(t, e.Report(context.Background(), ""), "boot_gates")
	if c.Status != proto.DoctorFail || !strings.Contains(c.Detail, "load gate") {
		t.Fatalf("closed load gate = %+v", c)
	}
	e.Gates = func(context.Context) (bool, bool, bool) { return true, false, true }
	c = find(t, e.Report(context.Background(), ""), "boot_gates")
	if c.Status != proto.DoctorFail || !strings.Contains(c.Detail, "disk gate") {
		t.Fatalf("closed disk gate = %+v", c)
	}
}

// TestReportJournal covers the journal dir checks.
func TestReportJournal(t *testing.T) {
	e := Env{Build: "test", Protocol: "v0"}
	if c := find(t, e.Report(context.Background(), ""), "journal"); c.Status != proto.DoctorWarn {
		t.Fatalf("disabled journal = %q, want warn", c.Status)
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	e.JournalDir = filepath.Join(dir, "journal")
	c := find(t, e.Report(context.Background(), ""), "journal")
	if c.Status != proto.DoctorFail || c.Remedy == "" {
		t.Fatalf("unwritable journal = %+v, want fail with a remedy", c)
	}
}

// TestReportDevices covers WDA probes, target filtering, and the
// unknown-target failure.
func TestReportDevices(t *testing.T) {
	cfg := proto.DevicesConfig{
		Enabled: true,
		WDA: map[string]proto.DeviceWDAConfig{
			"UP":   {URL: "http://127.0.0.1:8100"},
			"DOWN": {URL: "http://127.0.0.1:8101"},
		},
	}
	e := Env{
		Build: "test", Protocol: "v0", JournalDir: t.TempDir(),
		Devices: func() (proto.DevicesConfig, bool) { return cfg, true },
		WDAProbe: func(_ context.Context, url string) error {
			if strings.HasSuffix(url, "8100") {
				return nil
			}
			return errors.New("connection refused")
		},
	}
	r := e.Report(context.Background(), "")
	if r.OK {
		t.Fatal("report OK despite unreachable WDA")
	}
	if c := find(t, r, "wda:UP"); c.Status != proto.DoctorOK {
		t.Fatalf("wda:UP = %+v", c)
	}
	c := find(t, r, "wda:DOWN")
	if c.Status != proto.DoctorFail || !strings.Contains(c.Remedy, "iproxy") {
		t.Fatalf("wda:DOWN = %+v, want fail with the forward remedy", c)
	}

	// --target scopes device checks to one UDID.
	r = e.Report(context.Background(), "UP")
	if !r.OK {
		t.Fatalf("target-scoped report not OK: %+v", r.Checks)
	}
	for _, c := range r.Checks {
		if c.Name == "wda:DOWN" {
			t.Fatal("wda:DOWN checked despite --target UP")
		}
	}

	// Unknown target: an explicit failing check, not silence.
	r = e.Report(context.Background(), "NOPE")
	if c := find(t, r, "target"); c.Status != proto.DoctorFail {
		t.Fatalf("unknown target = %+v, want fail", c)
	}

	// Nothing configured: n/a with the onboard pointer, and a --target
	// still surfaces the unknown-target failure.
	e.Devices = func() (proto.DevicesConfig, bool) { return proto.DevicesConfig{}, true }
	if c := find(t, e.Report(context.Background(), ""), "devices"); c.Status != proto.DoctorNA ||
		!strings.Contains(c.Remedy, "device onboard") {
		t.Fatalf("no devices = %+v, want n/a with onboard remedy", c)
	}
	r = e.Report(context.Background(), "NOPE")
	if r.OK {
		t.Fatal("unknown target with no devices configured reported OK")
	}
	if c := find(t, r, "target"); c.Status != proto.DoctorFail {
		t.Fatalf("unknown target with no devices = %+v, want fail", c)
	}

	// Devices configured but disabled: probes skipped, n/a not fail.
	cfg.Enabled = false
	e.Devices = func() (proto.DevicesConfig, bool) { return cfg, true }
	e.WDAProbe = func(context.Context, string) error {
		t.Fatal("WDA probed despite devices disabled")
		return nil
	}
	r = e.Report(context.Background(), "")
	if !r.OK {
		t.Fatalf("disabled devices report not OK: %+v", r.Checks)
	}
	if c := find(t, r, "devices"); c.Status != proto.DoctorNA ||
		!strings.Contains(c.Detail, "disabled") {
		t.Fatalf("disabled devices = %+v, want n/a", c)
	}
	if c := find(t, e.Report(context.Background(), "NOPE"), "target"); c.Status != proto.DoctorFail {
		t.Fatalf("unknown target with devices disabled = %+v, want fail", c)
	}
	// A configured-but-disabled target is not "unknown": no target fail.
	r = e.Report(context.Background(), "UP")
	if !r.OK {
		t.Fatalf("configured target with devices disabled reported fail: %+v", r.Checks)
	}
	for _, c := range r.Checks {
		if c.Name == "target" {
			t.Fatalf("target check emitted for configured-but-disabled device: %+v", c)
		}
	}
}

// fakeMirrordDoctor serves GET /v1/doctor over a unix socket so the real
// mirror.Client transport is exercised, mirroring the fakeMirrord
// harness in internal/actions.
func fakeMirrordDoctor(t *testing.T, report map[string]any) string {
	t.Helper()
	// Not t.TempDir(): its per-test-name path blows past macOS's
	// 104-byte sockaddr_un limit.
	dir, err := os.MkdirTemp("", "m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{
		Listener: ln,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/doctor" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(report)
		})},
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return sock
}

// TestReportMirrorRoundTrip drives the mirror checks through the real
// mirror.Client against an in-process fake mirrord.
func TestReportMirrorRoundTrip(t *testing.T) {
	sock := fakeMirrordDoctor(t, map[string]any{
		"ok": false, "state": "ready", "input_mode": "focused",
		"background": false, "capture": "none", "verify": "off",
		"checks": []map[string]any{
			{"name": "background_input", "ok": true, "detail": "event record accepted"},
			{"name": "capture", "ok": false, "detail": "no capture: grant Screen Recording to /usr/local/bin/mirrord (responsible process)"},
		},
	})
	e := Env{
		Build: "test", Protocol: "v0", JournalDir: t.TempDir(),
		Devices: func() (proto.DevicesConfig, bool) {
			return proto.DevicesConfig{Enabled: true, Mirror: map[string]proto.DeviceMirrorConfig{
				"PHONE": {Socket: sock},
			}}, true
		},
		MirrorDoctor: func(ctx context.Context, socket string) (mirror.DoctorReport, error) {
			return mirror.New(socket).Doctor(ctx)
		},
	}
	r := e.Report(context.Background(), "")
	if r.OK {
		t.Fatal("report OK despite failed mirror capture probe")
	}
	if c := find(t, r, "mirror:PHONE"); c.Status != proto.DoctorFail ||
		!strings.Contains(c.Detail, "capture none") {
		t.Fatalf("mirror summary = %+v", c)
	}
	if c := find(t, r, "mirror:PHONE:background_input"); c.Status != proto.DoctorOK {
		t.Fatalf("mirror background_input = %+v", c)
	}
	c := find(t, r, "mirror:PHONE:capture")
	if c.Status != proto.DoctorFail ||
		!strings.Contains(c.Detail, "responsible process") ||
		!strings.Contains(c.Remedy, "Screen Recording") {
		t.Fatalf("mirror capture = %+v, want fail carrying the responsible process + TCC remedy", c)
	}

	// Helper socket gone: one failing check with the launch remedy.
	e.Devices = func() (proto.DevicesConfig, bool) {
		return proto.DevicesConfig{Enabled: true, Mirror: map[string]proto.DeviceMirrorConfig{
			"PHONE": {Socket: filepath.Join(t.TempDir(), "gone.sock")},
		}}, true
	}
	c = find(t, e.Report(context.Background(), ""), "mirror:PHONE")
	if c.Status != proto.DoctorFail || !strings.Contains(c.Remedy, "mirrord") {
		t.Fatalf("unreachable mirrord = %+v, want fail with the helper remedy", c)
	}
}
