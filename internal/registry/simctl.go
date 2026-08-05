package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/BariBariGood/manzanas/proto"
)

// SimctlRegistry enumerates iOS simulators via `xcrun simctl`. It only works
// on macOS hosts with Xcode command-line tools installed; use NewMock
// elsewhere.
type SimctlRegistry struct {
	// run executes a command and returns stdout; overridable for tests.
	run func(ctx context.Context, args ...string) ([]byte, error)
}

// NewSimctl returns a Registry backed by `xcrun simctl`.
func NewSimctl() *SimctlRegistry {
	return &SimctlRegistry{run: runXcrun}
}

func runXcrun(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "xcrun", append([]string{"simctl"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("simctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("simctl %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// simctlDevice mirrors one entry of `xcrun simctl list devices --json`.
type simctlDevice struct {
	UDID                 string `json:"udid"`
	Name                 string `json:"name"`
	State                string `json:"state"`
	IsAvailable          bool   `json:"isAvailable"`
	DeviceTypeIdentifier string `json:"deviceTypeIdentifier"`
}

type simctlList struct {
	Devices map[string][]simctlDevice `json:"devices"`
}

// runtimeName converts "com.apple.CoreSimulator.SimRuntime.iOS-26-5" to "iOS 26.5".
var runtimeIDRe = regexp.MustCompile(`SimRuntime\.([A-Za-z]+)-([0-9]+)-([0-9]+)$`)

func runtimeName(id string) string {
	if m := runtimeIDRe.FindStringSubmatch(id); m != nil {
		return fmt.Sprintf("%s %s.%s", m[1], m[2], m[3])
	}
	return id
}

// deviceTypeName converts "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro"
// to "iPhone 17 Pro".
func deviceTypeName(id string) string {
	last := id[strings.LastIndex(id, ".")+1:]
	return strings.ReplaceAll(last, "-", " ")
}

func (r *SimctlRegistry) List(ctx context.Context) ([]proto.Target, error) {
	out, err := r.run(ctx, "list", "devices", "--json")
	if err != nil {
		return nil, err
	}
	var parsed simctlList
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parse simctl output: %w", err)
	}
	var targets []proto.Target
	for runtimeID, devices := range parsed.Devices {
		rt := runtimeName(runtimeID)
		for _, d := range devices {
			if !d.IsAvailable {
				continue
			}
			// Hidden clone devices backing state-engine snapshots and
			// transient golden-image builder sims are not leasable targets.
			if strings.HasPrefix(d.Name, proto.SnapshotDeviceNamePrefix) ||
				strings.HasPrefix(d.Name, proto.ImageDeviceNamePrefix) {
				continue
			}
			dt := deviceTypeName(d.DeviceTypeIdentifier)
			targets = append(targets, proto.Target{
				UDID:       d.UDID,
				Kind:       proto.TargetSimulator,
				Name:       d.Name,
				Runtime:    rt,
				DeviceType: dt,
				State:      toState(d.State),
				Labels:     DeriveLabels(string(proto.TargetSimulator), rt, dt),
			})
		}
	}
	return targets, nil
}

func toState(s string) proto.TargetState {
	switch s {
	case "Shutdown", "Booted", "Booting", "ShuttingDown":
		return proto.TargetState(s)
	default:
		return proto.StateUnknown
	}
}

func (r *SimctlRegistry) Get(ctx context.Context, udid string) (proto.Target, error) {
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

func (r *SimctlRegistry) Boot(ctx context.Context, udid string) error {
	_, err := r.run(ctx, "boot", udid)
	if err != nil && strings.Contains(err.Error(), "current state: Booted") {
		return nil // already booted
	}
	return err
}

func (r *SimctlRegistry) Shutdown(ctx context.Context, udid string) error {
	_, err := r.run(ctx, "shutdown", udid)
	if err != nil && strings.Contains(err.Error(), "current state: Shutdown") {
		return nil // already shut down
	}
	return err
}

func (r *SimctlRegistry) Health(ctx context.Context, udid string) (proto.TargetState, error) {
	t, err := r.Get(ctx, udid)
	if err != nil {
		return proto.StateUnknown, err
	}
	return t.State, nil
}
