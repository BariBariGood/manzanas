// Package devices owns runtime device configuration: the devices config
// file, the flag-pile equivalent, and the manager that applies a config to
// a running daemon (registry toggle, WDA clients, runner/forward
// supervisors) without a restart.
package devices

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BariBariGood/manzanas/internal/actions/wda"
	"github.com/BariBariGood/manzanas/proto"
)

// ParsePairs parses a comma-separated "<udid>=<value>" flag (the shape of
// --device-wda, --device-wda-launch, and --device-wda-forward).
func ParsePairs(flagName, spec string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		udid, val, ok := strings.Cut(pair, "=")
		if !ok || udid == "" || val == "" {
			return nil, fmt.Errorf("invalid %s entry %q (want <udid>=<value>)", flagName, pair)
		}
		out[udid] = val
	}
	return out, nil
}

// DefaultMirrorSocket is the default unix socket path of the mirrord
// GUI helper (see helpers/mirrord), used when a mirror-backed device's
// config leaves the socket empty.
func DefaultMirrorSocket() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "mirrord.sock"
	}
	return filepath.Join(home, ".manzanasd", "mirrord.sock")
}

// ParseBackends parses the --device-backend flag: comma-separated
// "<udid>=wda|mirror" pairs. Devices without an entry default to wda.
// Harmless duplicate entries are deduplicated; conflicting redefinitions
// of the same UDID are rejected.
func ParseBackends(spec string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		udid, backend, ok := strings.Cut(pair, "=")
		if !ok || udid == "" || (backend != "wda" && backend != "mirror") {
			return nil, fmt.Errorf("invalid --device-backend entry %q (want <udid>=wda|mirror)", pair)
		}
		if prev, dup := out[udid]; dup && prev != backend {
			return nil, fmt.Errorf("--device-backend lists device %s twice with conflicting backends (%s and %s)", udid, prev, backend)
		}
		out[udid] = backend
	}
	return out, nil
}

// FromFlags assembles a DevicesConfig from the classic flag pile
// (--devices, --device-wda, --device-wda-launch, --device-wda-forward,
// --device-backend, --device-mirror-socket), kept for backwards
// compatibility with pre-config-file deployments.
func FromFlags(enabled bool, wdaSpec, launchSpec, forwardSpec, backendSpec, mirrorSocket string) (proto.DevicesConfig, error) {
	cfg := proto.DevicesConfig{Enabled: enabled, WDA: map[string]proto.DeviceWDAConfig{}}
	urls, err := ParsePairs("--device-wda", wdaSpec)
	if err != nil {
		return cfg, err
	}
	launches, err := ParsePairs("--device-wda-launch", launchSpec)
	if err != nil {
		return cfg, err
	}
	forwards, err := ParsePairs("--device-wda-forward", forwardSpec)
	if err != nil {
		return cfg, err
	}
	backends, err := ParseBackends(backendSpec)
	if err != nil {
		return cfg, err
	}
	for udid, u := range urls {
		cfg.WDA[udid] = proto.DeviceWDAConfig{URL: u}
	}
	for udid, l := range launches {
		d := cfg.WDA[udid]
		d.Launch = l
		cfg.WDA[udid] = d
	}
	for udid, f := range forwards {
		d := cfg.WDA[udid]
		d.Forward = f
		cfg.WDA[udid] = d
	}
	for udid, kind := range backends {
		if kind != "mirror" {
			continue
		}
		if cfg.Mirror == nil {
			cfg.Mirror = map[string]proto.DeviceMirrorConfig{}
		}
		cfg.Mirror[udid] = proto.DeviceMirrorConfig{Socket: mirrorSocket}
	}
	return cfg, Validate(cfg)
}

// Load reads a devices config file (JSON, shape proto.DevicesConfig; see
// docs/devices.md) and validates it.
func Load(path string) (proto.DevicesConfig, error) {
	var cfg proto.DevicesConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("devices config %s: %w", path, err)
	}
	if err := Validate(cfg); err != nil {
		return cfg, fmt.Errorf("devices config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks a DevicesConfig: every launch/forward spec must parse
// and needs a matching URL (a supervisor without an endpoint to probe is
// meaningless).
func Validate(cfg proto.DevicesConfig) error {
	// Cross-device collisions are silently wrong at runtime: a duplicate
	// forward local port means the second device's health probe sees the
	// first device's listener (and never starts its own forwarder), and a
	// duplicate URL routes one device's actions to the other's phone.
	urls := map[string]string{}
	locals := map[int]string{}
	for udid, d := range cfg.WDA {
		if udid == "" {
			return fmt.Errorf("devices config: empty device UDID")
		}
		if d.URL == "" && (d.Launch != "" || d.Forward != "") {
			return fmt.Errorf("device %s: launch/forward configured without a wda url", udid)
		}
		if d.Launch != "" {
			if _, err := wda.ParseLauncher(udid, d.Launch); err != nil {
				return fmt.Errorf("device %s: %w", udid, err)
			}
		}
		if d.Forward != "" {
			fwd, err := wda.ParseForward(udid, d.Forward)
			if err != nil {
				return fmt.Errorf("device %s: %w", udid, err)
			}
			if other, ok := locals[fwd.Local]; ok {
				return fmt.Errorf("devices %s and %s: duplicate forward local port %d", other, udid, fwd.Local)
			}
			locals[fwd.Local] = udid
		}
		if d.URL != "" {
			if other, ok := urls[d.URL]; ok {
				return fmt.Errorf("devices %s and %s: duplicate wda url %s", other, udid, d.URL)
			}
			urls[d.URL] = udid
		}
	}
	// The mirror is exclusive global state: macOS iPhone Mirroring shows
	// one phone per Mac and its window is the single shared input/capture
	// surface, so at most one device may be mirror-backed. A device is
	// mirror-backed or WDA-backed, never both: a stray WDA entry would
	// race the mirror for the phone.
	if len(cfg.Mirror) > 1 {
		return fmt.Errorf("devices config allows at most one mirror-backed device: iPhone Mirroring shows one phone per Mac and its window is exclusive global state")
	}
	for udid := range cfg.Mirror {
		if udid == "" {
			return fmt.Errorf("devices config: empty mirror device UDID")
		}
		if _, ok := cfg.WDA[udid]; ok {
			return fmt.Errorf("device %s: mirror backend conflicts with its wda entry; pick one backend per device", udid)
		}
	}
	return nil
}

// UDIDs returns the config's device UDIDs (WDA and mirror), sorted
// (stable logs/output).
func UDIDs(cfg proto.DevicesConfig) []string {
	out := make([]string, 0, len(cfg.WDA)+len(cfg.Mirror))
	for u := range cfg.WDA {
		out = append(out, u)
	}
	for u := range cfg.Mirror {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// WriteFile writes cfg to path as indented JSON (0600: the file lives in
// the daemon's state dir and there is no reason to share it).
func WriteFile(path string, cfg proto.DevicesConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
