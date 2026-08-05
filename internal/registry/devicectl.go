package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BariBariGood/manzanas/proto"
)

// DevicectlRegistry enumerates physical devices via `xcrun devicectl`. It
// only works on macOS hosts with Xcode 15+ command-line tools installed.
// Devices cannot be booted or shut down through the registry: connection
// state (the CoreDevice tunnel) maps onto TargetState instead.
type DevicectlRegistry struct {
	// run executes a devicectl subcommand and returns its JSON output;
	// overridable for tests.
	run func(ctx context.Context, args ...string) ([]byte, error)
}

// NewDevicectl returns a Registry backed by `xcrun devicectl`.
func NewDevicectl() *DevicectlRegistry {
	return &DevicectlRegistry{run: runDevicectl}
}

// runDevicectl executes `xcrun devicectl <args> --json-output <tmpfile>`
// and returns the file's contents (devicectl writes JSON to a file, not
// stdout).
func runDevicectl(ctx context.Context, args ...string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "manzanasd-devicectl-")
	if err != nil {
		return nil, fmt.Errorf("devicectl temp dir: %w", err)
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "out.json")
	argv := append([]string{"devicectl"}, args...)
	argv = append(argv, "--json-output", out, "--timeout", "20")
	cmd := exec.CommandContext(ctx, "xcrun", argv...)
	if _, err := cmd.Output(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("devicectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("devicectl %s: %w", strings.Join(args, " "), err)
	}
	return os.ReadFile(out)
}

// devicectlDevice mirrors one entry of `devicectl list devices` JSON.
type devicectlDevice struct {
	Identifier           string `json:"identifier"`
	VisibilityClass      string `json:"visibilityClass"`
	ConnectionProperties struct {
		PairingState  string `json:"pairingState"`
		TransportType string `json:"transportType"`
		TunnelState   string `json:"tunnelState"`
	} `json:"connectionProperties"`
	DeviceProperties struct {
		Name                string `json:"name"`
		OSVersionNumber     string `json:"osVersionNumber"`
		DeveloperModeStatus string `json:"developerModeStatus"`
	} `json:"deviceProperties"`
	HardwareProperties struct {
		UDID          string `json:"udid"`
		MarketingName string `json:"marketingName"`
		ProductType   string `json:"productType"`
		Platform      string `json:"platform"`
		Reality       string `json:"reality"`
	} `json:"hardwareProperties"`
}

type devicectlList struct {
	Result struct {
		Devices []devicectlDevice `json:"devices"`
	} `json:"result"`
}

// DisconnectedLabel is added to device targets whose CoreDevice tunnel is
// down: they are visible and leasable, but connection-requiring actions
// fail until the device is plugged in (or reachable over Wi-Fi).
const DisconnectedLabel = "disconnected"

func (r *DevicectlRegistry) List(ctx context.Context) ([]proto.Target, error) {
	out, err := r.run(ctx, "list", "devices")
	if err != nil {
		return nil, err
	}
	var parsed devicectlList
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parse devicectl output: %w", err)
	}
	var targets []proto.Target
	for _, d := range parsed.Result.Devices {
		if d.HardwareProperties.UDID == "" {
			continue
		}
		if d.VisibilityClass != "" && d.VisibilityClass != "default" {
			continue
		}
		rt := strings.TrimSpace(d.HardwareProperties.Platform + " " + d.DeviceProperties.OSVersionNumber)
		dt := d.HardwareProperties.MarketingName
		if dt == "" {
			dt = d.HardwareProperties.ProductType
		}
		connected := d.ConnectionProperties.TunnelState == "connected"
		state := proto.StateUnknown
		labels := DeriveLabels(string(proto.TargetDevice), rt, dt)
		if connected {
			state = proto.StateBooted
		} else {
			labels = append(labels, DisconnectedLabel)
		}
		targets = append(targets, proto.Target{
			UDID:       d.HardwareProperties.UDID,
			Kind:       proto.TargetDevice,
			Name:       d.DeviceProperties.Name,
			Runtime:    rt,
			DeviceType: dt,
			State:      state,
			Labels:     labels,
		})
	}
	return targets, nil
}

func (r *DevicectlRegistry) Get(ctx context.Context, udid string) (proto.Target, error) {
	targets, err := r.List(ctx)
	if err != nil {
		return proto.Target{}, err
	}
	for _, t := range targets {
		if t.UDID == udid {
			return t, nil
		}
	}
	return proto.Target{}, &NotFoundError{UDID: udid}
}

// DeviceBootError is returned when a caller asks the registry to boot a
// physical device: devices cannot be booted remotely; the CoreDevice
// tunnel comes up when the device is connected over USB or Wi-Fi.
type DeviceBootError struct{ UDID string }

func (e *DeviceBootError) Error() string {
	return "cannot boot physical device " + e.UDID + "; connect it over USB or Wi-Fi to bring the tunnel up"
}

func (r *DevicectlRegistry) Boot(ctx context.Context, udid string) error {
	if _, err := r.Get(ctx, udid); err != nil {
		return err
	}
	return &DeviceBootError{UDID: udid}
}

// DeviceShutdownError is returned when a caller asks the registry to shut
// down a physical device: powering off someone's phone as a side effect
// of lease bookkeeping would be hostile, and pretending success would let
// clients believe the device state changed.
type DeviceShutdownError struct{ UDID string }

func (e *DeviceShutdownError) Error() string {
	return "cannot shut down physical device " + e.UDID + "; power it off from the device itself"
}

func (r *DevicectlRegistry) Shutdown(ctx context.Context, udid string) error {
	if _, err := r.Get(ctx, udid); err != nil {
		return err
	}
	return &DeviceShutdownError{UDID: udid}
}

func (r *DevicectlRegistry) Health(ctx context.Context, udid string) (proto.TargetState, error) {
	t, err := r.Get(ctx, udid)
	if err != nil {
		return proto.StateUnknown, err
	}
	return t.State, nil
}
