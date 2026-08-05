package state

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner abstracts the simctl binary and the CoreSimulator filesystem so
// the engine is testable on Linux with a fake.
type Runner interface {
	// Simctl runs `xcrun simctl <args...>` and returns stdout.
	Simctl(ctx context.Context, args ...string) ([]byte, error)
	// SimctlInput runs `xcrun simctl <args...>` with stdin.
	SimctlInput(ctx context.Context, stdin []byte, args ...string) ([]byte, error)
	// DeviceDataDir returns the device's data directory
	// (~/Library/Developer/CoreSimulator/Devices/<udid>/data).
	DeviceDataDir(udid string) string
	// ReplaceDir removes dst and copies src to dst (APFS clonefile when
	// possible). The copy is bounded by ctx.
	ReplaceDir(ctx context.Context, src, dst string) error
}

// hostRunner is the real Runner backed by xcrun simctl and cp.
type hostRunner struct {
	devicesDir string
}

// NewHostRunner returns a Runner for the local macOS host. devicesDir may
// be empty to use the default CoreSimulator devices directory.
func NewHostRunner(devicesDir string) Runner {
	if devicesDir == "" {
		home, _ := os.UserHomeDir()
		devicesDir = filepath.Join(home, "Library", "Developer", "CoreSimulator", "Devices")
	}
	return &hostRunner{devicesDir: devicesDir}
}

func (h *hostRunner) Simctl(ctx context.Context, args ...string) ([]byte, error) {
	return h.SimctlInput(ctx, nil, args...)
}

func (h *hostRunner) SimctlInput(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "xcrun", append([]string{"simctl"}, args...)...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("simctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("simctl %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func (h *hostRunner) DeviceDataDir(udid string) string {
	return filepath.Join(h.devicesDir, udid, "data")
}

// ReplaceDir replaces dst with a copy of src. The copy is staged into a
// sibling temp directory first and swapped in with renames, so a failed
// copy never leaves dst missing or partial. `cp -c` requests an APFS
// clonefile copy (instant, space-shared); when it fails — filesystems
// without clonefile support, or sims that ever booted, whose
// system-protected cache files (e.g. locationd's locScoreInfo) make cp
// die with EPERM — it falls back to a tolerant tree copy that skips
// unreadable entries.
func (h *hostRunner) ReplaceDir(ctx context.Context, src, dst string) error {
	staging := dst + ".manzanasd-staging"
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("clear staging %s: %w", staging, err)
	}
	if out, err := exec.CommandContext(ctx, "cp", "-c", "-R", src, staging).CombinedOutput(); err != nil {
		_ = os.RemoveAll(staging)
		if _, err2 := copyTreeTolerant(ctx, src, staging); err2 != nil {
			_ = os.RemoveAll(staging)
			return fmt.Errorf("copy %s -> %s: clonefile: %v: %s / fallback: %w", src, staging,
				err, strings.TrimSpace(string(out)), err2)
		}
	}
	old := dst + ".manzanasd-old"
	if err := os.RemoveAll(old); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("clear %s: %w", old, err)
	}
	replaced := false
	if err := os.Rename(dst, old); err != nil {
		if !os.IsNotExist(err) {
			_ = os.RemoveAll(staging)
			return fmt.Errorf("move aside %s: %w", dst, err)
		}
	} else {
		replaced = true
	}
	if err := os.Rename(staging, dst); err != nil {
		if replaced {
			_ = os.Rename(old, dst) // roll back
		}
		_ = os.RemoveAll(staging)
		return fmt.Errorf("swap in %s: %w", dst, err)
	}
	if replaced {
		_ = os.RemoveAll(old)
	}
	return nil
}
