// Package doctor aggregates host setup diagnostics for `manzanas doctor`
// (GET /v0/doctor): daemon basics, the simulator toolchain, devicectl,
// per-device WDA reachability, and the mirrord helper's capability
// probes. Every check carries an actionable one-line remedy, and checks
// that cannot apply to the host (simulator tooling on Linux or under
// --mock) report "n/a" rather than failing, so the doctor is useful on a
// fresh Mac with nothing configured and on a Linux dev box alike.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
	"github.com/BariBariGood/manzanas/proto"
)

// probeTimeout bounds each external probe (a WDA HTTP round-trip, one
// mirrord socket call) so a wedged endpoint turns into a fail line
// instead of hanging the whole report.
const probeTimeout = 5 * time.Second

// Env is everything the doctor needs to probe a host. Function fields
// are injectable so aggregation and rendering are unit-testable with
// fake results; New fills in the real implementations.
type Env struct {
	// Build is the daemon's build version; Protocol the wire version.
	Build    string
	Protocol string
	// SimTooling reports whether the host is expected to have the
	// simulator toolchain at all: false on Linux and under --mock,
	// where sim/devicectl checks report "n/a" rather than fail.
	SimTooling bool
	// JournalDir is the run journal directory; empty means the journal
	// is disabled.
	JournalDir string

	// LookPath resolves a binary on PATH (exec.LookPath).
	LookPath func(name string) (string, error)
	// RunOut runs a host command and returns its stdout.
	RunOut func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Devices returns the daemon's runtime device configuration; ok is
	// false when the daemon has no device manager (mock builds).
	Devices func() (cfg proto.DevicesConfig, ok bool)
	// Gates snapshots the warm pool's boot-safety gates; ok is false
	// when the daemon runs without a pool.
	Gates func(ctx context.Context) (loadOK, diskOK, ok bool)
	// MirrorDoctor runs the mirrord helper's capability probes over its
	// unix socket.
	MirrorDoctor func(ctx context.Context, socket string) (mirror.DoctorReport, error)
	// WDAProbe checks a device's WebDriverAgent endpoint; nil error
	// means reachable and ready.
	WDAProbe func(ctx context.Context, url string) error
}

// New returns an Env with the real host probes wired, ready for the
// caller to fill in the daemon-specific fields (Devices, Gates).
func New(build, protocol string, simTooling bool, journalDir string,
	lookPath func(string) (string, error),
	runOut func(context.Context, string, ...string) ([]byte, error)) Env {
	return Env{
		Build: build, Protocol: protocol,
		SimTooling: simTooling, JournalDir: journalDir,
		LookPath: lookPath, RunOut: runOut,
		MirrorDoctor: func(ctx context.Context, socket string) (mirror.DoctorReport, error) {
			ctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			return mirror.New(socket).Doctor(ctx)
		},
		WDAProbe: probeWDA,
	}
}

// probeWDA GETs the WDA /status route; WebDriverAgent answers it on
// every session-less connection, so a 200 means the runner is up and
// the forward (if any) carries traffic.
func probeWDA(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(url, "/")+"/status", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("WDA /status answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// Report runs every applicable check. target, when non-empty, restricts
// the per-device checks (WDA, mirror) to that UDID; host-level checks
// always run.
func (e Env) Report(ctx context.Context, target string) proto.DoctorReport {
	r := proto.DoctorReport{Build: e.Build, Protocol: e.Protocol}
	r.Checks = append(r.Checks, proto.DoctorCheck{
		Name: "daemon", Status: proto.DoctorOK,
		Detail: "manzanasd " + e.Build + " (protocol " + e.Protocol + ")",
	})
	r.Checks = append(r.Checks, e.checkSimToolchain(ctx)...)
	r.Checks = append(r.Checks, e.checkJournal())
	r.Checks = append(r.Checks, e.checkGates(ctx))
	r.Checks = append(r.Checks, e.checkDevices(ctx, target)...)
	r.OK = true
	for _, c := range r.Checks {
		if c.Status == proto.DoctorFail {
			r.OK = false
			break
		}
	}
	return r
}

// notApplicable is the shared n/a for hosts without simulator tooling
// (Linux, --mock).
func notApplicable(name, why string) proto.DoctorCheck {
	return proto.DoctorCheck{Name: name, Status: proto.DoctorNA, Detail: "not applicable: " + why}
}

func (e Env) checkSimToolchain(ctx context.Context) []proto.DoctorCheck {
	if !e.SimTooling {
		why := "no simulator toolchain on this host (mock registry / not macOS)"
		return []proto.DoctorCheck{
			notApplicable("xcrun", why),
			notApplicable("ios_runtime", why),
			notApplicable("devicectl", why),
		}
	}
	var out []proto.DoctorCheck
	xcrunPath, err := e.LookPath("xcrun")
	if err != nil {
		out = append(out, proto.DoctorCheck{
			Name: "xcrun", Status: proto.DoctorFail,
			Detail: "xcrun not found on PATH",
			Remedy: "install Xcode from the App Store, then `sudo xcode-select -s /Applications/Xcode.app`",
		})
		// Everything below shells out through xcrun; report the root
		// cause once instead of three identical failures.
		out = append(out,
			proto.DoctorCheck{Name: "ios_runtime", Status: proto.DoctorFail,
				Detail: "cannot check: xcrun missing", Remedy: "fix the xcrun check first"},
			proto.DoctorCheck{Name: "devicectl", Status: proto.DoctorFail,
				Detail: "cannot check: xcrun missing", Remedy: "fix the xcrun check first"})
		return out
	}
	out = append(out, proto.DoctorCheck{
		Name: "xcrun", Status: proto.DoctorOK, Detail: xcrunPath,
	})
	out = append(out, e.checkRuntimes(ctx))
	if _, err := e.RunOut(ctx, "xcrun", "devicectl", "--version"); err != nil {
		out = append(out, proto.DoctorCheck{
			Name: "devicectl", Status: proto.DoctorWarn,
			Detail: "xcrun devicectl failed: " + firstLine(err.Error()),
			Remedy: "physical-device features need Xcode 15+ (`xcrun devicectl --version`); simulators are unaffected",
		})
	} else {
		out = append(out, proto.DoctorCheck{
			Name: "devicectl", Status: proto.DoctorOK, Detail: "xcrun devicectl available",
		})
	}
	return out
}

// checkRuntimes asks simctl for installed runtimes and requires at least
// one available iOS runtime.
func (e Env) checkRuntimes(ctx context.Context) proto.DoctorCheck {
	b, err := e.RunOut(ctx, "xcrun", "simctl", "list", "runtimes", "-j")
	if err != nil {
		return proto.DoctorCheck{
			Name: "ios_runtime", Status: proto.DoctorFail,
			Detail: "xcrun simctl failed: " + firstLine(err.Error()),
			Remedy: "install Xcode (the command-line tools alone have no simulators), then `sudo xcode-select -s /Applications/Xcode.app`",
		}
	}
	var parsed struct {
		Runtimes []struct {
			Name        string `json:"name"`
			IsAvailable bool   `json:"isAvailable"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		return proto.DoctorCheck{
			Name: "ios_runtime", Status: proto.DoctorFail,
			Detail: "cannot parse simctl runtimes output: " + err.Error(),
			Remedy: "run `xcrun simctl list runtimes` by hand and check the Xcode install",
		}
	}
	var available []string
	for _, rt := range parsed.Runtimes {
		if rt.IsAvailable && strings.HasPrefix(rt.Name, "iOS") {
			available = append(available, rt.Name)
		}
	}
	if len(available) == 0 {
		return proto.DoctorCheck{
			Name: "ios_runtime", Status: proto.DoctorFail,
			Detail: "no available iOS simulator runtime installed",
			Remedy: "install one: `xcodebuild -downloadPlatform iOS` (or Xcode ▸ Settings ▸ Components)",
		}
	}
	return proto.DoctorCheck{
		Name: "ios_runtime", Status: proto.DoctorOK,
		Detail: strings.Join(available, ", "),
	}
}

// checkJournal verifies the run journal directory is writable — every
// action's evidence lands there, so an unwritable dir silently degrades
// runs.
func (e Env) checkJournal() proto.DoctorCheck {
	if e.JournalDir == "" {
		return proto.DoctorCheck{
			Name: "journal", Status: proto.DoctorWarn,
			Detail: "run journal disabled (no journal dir)",
			Remedy: "start manzanasd with --journal-dir (default ~/.manzanasd/journal) to record run evidence",
		}
	}
	if err := os.MkdirAll(e.JournalDir, 0o755); err != nil {
		return proto.DoctorCheck{
			Name: "journal", Status: proto.DoctorFail,
			Detail: "journal dir not creatable: " + err.Error(),
			Remedy: "fix permissions on " + e.JournalDir + " (or point --journal-dir elsewhere)",
		}
	}
	probe, err := os.CreateTemp(e.JournalDir, ".doctor-*")
	if err != nil {
		return proto.DoctorCheck{
			Name: "journal", Status: proto.DoctorFail,
			Detail: "journal dir not writable: " + err.Error(),
			Remedy: "fix permissions on " + e.JournalDir + " (or point --journal-dir elsewhere)",
		}
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)
	return proto.DoctorCheck{
		Name: "journal", Status: proto.DoctorOK, Detail: e.JournalDir + " writable",
	}
}

// checkGates reports the warm pool's boot-safety gates: a false gate
// means a cold simulator boot would be refused right now.
func (e Env) checkGates(ctx context.Context) proto.DoctorCheck {
	if e.Gates == nil {
		return notApplicable("boot_gates", "no warm pool on this daemon")
	}
	loadOK, diskOK, ok := e.Gates(ctx)
	if !ok {
		return notApplicable("boot_gates", "no warm pool on this daemon")
	}
	switch {
	case !loadOK && !diskOK:
		return proto.DoctorCheck{
			Name: "boot_gates", Status: proto.DoctorFail,
			Detail: "load and disk gates closed: cold boots are refused",
			Remedy: "wait for load to drop and free disk space (see docs/troubleshooting.md, `503 overloaded`)",
		}
	case !loadOK:
		return proto.DoctorCheck{
			Name: "boot_gates", Status: proto.DoctorFail,
			Detail: "load gate closed: cold boots are refused",
			Remedy: "wait for host load to drop, or shut idle simulators down",
		}
	case !diskOK:
		return proto.DoctorCheck{
			Name: "boot_gates", Status: proto.DoctorFail,
			Detail: "disk gate closed: cold boots are refused",
			Remedy: "free disk space (simulator images and journals are the usual suspects)",
		}
	}
	return proto.DoctorCheck{
		Name: "boot_gates", Status: proto.DoctorOK, Detail: "load and disk gates open",
	}
}

// checkDevices runs the per-device probes: WDA reachability for
// WDA-backed devices and the mirrord capability report for the
// mirror-backed one.
func (e Env) checkDevices(ctx context.Context, target string) []proto.DoctorCheck {
	if e.Devices == nil {
		return []proto.DoctorCheck{notApplicable("devices", "no device manager on this daemon")}
	}
	cfg, ok := e.Devices()
	if !ok {
		return []proto.DoctorCheck{notApplicable("devices", "no device manager on this daemon")}
	}
	if !cfg.Enabled && (len(cfg.WDA) > 0 || len(cfg.Mirror) > 0) {
		// Disabled on purpose: the manager runs no WDA supervisors and
		// wires no mirror client, so live probes would fail against
		// endpoints nobody is serving — not a setup problem.
		out := []proto.DoctorCheck{{
			Name: "devices", Status: proto.DoctorNA,
			Detail: "physical device targets disabled in the devices config (probes skipped)",
			Remedy: "enable them with `enabled: true` via POST /v0/devices when needed (see docs/devices.md)",
		}}
		if target != "" && !configuredTarget(cfg, target) {
			out = append(out, unknownTarget(target))
		}
		return out
	}
	if len(cfg.WDA) == 0 && len(cfg.Mirror) == 0 {
		out := []proto.DoctorCheck{{
			Name: "devices", Status: proto.DoctorNA,
			Detail: "no physical devices configured",
			Remedy: "optional — wire a phone with `manzanas device onboard <udid>` (see docs/devices.md)",
		}}
		if target != "" {
			out = append(out, unknownTarget(target))
		}
		return out
	}
	var out []proto.DoctorCheck
	matched := target == ""
	for _, udid := range sortedKeys(cfg.WDA) {
		if target != "" && udid != target {
			continue
		}
		matched = true
		out = append(out, e.checkWDA(ctx, udid, cfg.WDA[udid]))
	}
	for _, udid := range sortedKeys(cfg.Mirror) {
		if target != "" && udid != target {
			continue
		}
		matched = true
		out = append(out, e.checkMirror(ctx, udid, cfg.Mirror[udid])...)
	}
	if !matched {
		out = append(out, unknownTarget(target))
	}
	return out
}

// configuredTarget reports whether a UDID appears in the devices config.
func configuredTarget(cfg proto.DevicesConfig, udid string) bool {
	_, w := cfg.WDA[udid]
	_, m := cfg.Mirror[udid]
	return w || m
}

// unknownTarget is the failing check for a --target that matches no
// configured device — a typo must never yield an all-green report.
func unknownTarget(target string) proto.DoctorCheck {
	return proto.DoctorCheck{
		Name: "target", Status: proto.DoctorFail,
		Detail: "no configured device matches target " + target,
		Remedy: "check `manzanas targets` and `manzanas device config` for the UDID",
	}
}

func (e Env) checkWDA(ctx context.Context, udid string, d proto.DeviceWDAConfig) proto.DoctorCheck {
	name := "wda:" + udid
	if d.URL == "" {
		return proto.DoctorCheck{
			Name: name, Status: proto.DoctorFail,
			Detail: "device configured without a WDA URL",
			Remedy: "set the device's wda url (`manzanas device onboard " + udid + "` writes a full config)",
		}
	}
	if err := e.WDAProbe(ctx, d.URL); err != nil {
		remedy := "check the WDA runner on the phone and the usbmux forward (iproxy)"
		if d.Launch == "" {
			remedy += "; a `launch` spec lets the daemon relaunch WDA itself (docs/devices.md)"
		}
		return proto.DoctorCheck{
			Name: name, Status: proto.DoctorFail,
			Detail: "WDA unreachable at " + d.URL + ": " + firstLine(err.Error()),
			Remedy: remedy,
		}
	}
	return proto.DoctorCheck{
		Name: name, Status: proto.DoctorOK, Detail: "WDA ready at " + d.URL,
	}
}

// checkMirror maps the mirrord helper's probe report onto doctor checks.
// The helper probes for real (a capture, an event record) and its detail
// lines name the responsible process for a missing TCC grant, so they
// are surfaced verbatim.
func (e Env) checkMirror(ctx context.Context, udid string, d proto.DeviceMirrorConfig) []proto.DoctorCheck {
	name := "mirror:" + udid
	socket := d.Socket
	if socket == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			socket = filepath.Join(home, ".manzanasd", "mirrord.sock")
		}
	}
	rep, err := e.MirrorDoctor(ctx, socket)
	if err != nil {
		return []proto.DoctorCheck{{
			Name: name, Status: proto.DoctorFail,
			Detail: "mirrord unreachable: " + firstLine(err.Error()),
			Remedy: "start the mirrord helper in the GUI session (helpers/mirrord, `mirrord --install-service`) serving " + socket,
		}}
	}
	status := proto.DoctorOK
	if !rep.OK {
		status = proto.DoctorFail
	}
	out := []proto.DoctorCheck{{
		Name: name, Status: status,
		Detail: fmt.Sprintf("state %s; input %s; capture %s; verify %s",
			rep.State, rep.InputMode, rep.Capture, rep.Verify),
	}}
	for _, c := range rep.Checks {
		dc := proto.DoctorCheck{Name: name + ":" + c.Name, Detail: c.Detail}
		if c.OK {
			dc.Status = proto.DoctorOK
		} else {
			dc.Status = proto.DoctorFail
			dc.Remedy = mirrorRemedy(c.Name)
		}
		out = append(out, dc)
	}
	return out
}

// mirrorRemedy maps a failed mirrord probe to its fix. The helper's
// detail line names the responsible process — the binary the TCC grant
// must go to — so remedies point at the detail rather than guessing.
func mirrorRemedy(check string) string {
	switch {
	case strings.Contains(check, "capture"):
		return "grant Screen Recording (System Settings ▸ Privacy & Security) to the mirrord helper binary (named in the detail; default ~/bin/mirrord), then restart mirrord"
	case strings.Contains(check, "input"):
		return "grant Accessibility (System Settings ▸ Privacy & Security) to the mirrord helper binary (named in the detail; default ~/bin/mirrord), then restart mirrord"
	case strings.Contains(check, "session"), strings.Contains(check, "window"):
		return "open iPhone Mirroring on the Mac and connect the phone (unlock it if an interstitial is up)"
	default:
		return "see the detail line and docs/troubleshooting.md"
	}
}

// firstLine trims an error to its first line so multi-line tool output
// doesn't wreck the table.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
