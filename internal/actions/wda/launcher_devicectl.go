package wda

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// DevicectlLauncher starts a WebDriverAgentRunner that is already
// installed on the device (one-time `xcodebuild build-for-testing` +
// install with valid signing) via
// `xcrun devicectl device process launch --terminate-existing`.
// The runner keeps running on the device; there is nothing host-side to
// stop.
type DevicectlLauncher struct {
	UDID     string
	BundleID string
	// run executes one command to completion; overridable for tests.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// NewDevicectlLauncher builds a launcher for a pre-installed WDA runner
// bundle (e.g. "com.example.WebDriverAgentRunner.xctrunner").
func NewDevicectlLauncher(udid, bundleID string) *DevicectlLauncher {
	return &DevicectlLauncher{UDID: udid, BundleID: bundleID, run: runCommand}
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return out, err
}

// Launch implements Launcher.
func (l *DevicectlLauncher) Launch(ctx context.Context) error {
	out, err := l.run(ctx, "xcrun", "devicectl", "device", "process", "launch",
		"--terminate-existing", "--device", l.UDID, l.BundleID)
	if err != nil {
		return fmt.Errorf("devicectl launch %s on %s: %w: %s",
			l.BundleID, l.UDID, err, firstLine(out))
	}
	return nil
}

// Stop implements Launcher; the runner lives on the device, so there is
// no host-side process to reap.
func (l *DevicectlLauncher) Stop() {}

func (l *DevicectlLauncher) String() string {
	return "devicectl:" + l.BundleID
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(s)
}
