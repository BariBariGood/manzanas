package state

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var slimProfileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// HostSlimFunc returns a SlimFunc backed by the simslim binary
// (github.com/MobAI-App/simslim), or nil if none is installed. Search
// order: $MANZANASD_SIMSLIM, ~/bin/simslim, ~/simtest/bin/simslim, $PATH.
// simslim >= v0.6.0 fails loudly when disable overrides do not survive
// the reboot; the wrapped error carries simslim's combined output so the
// failed daemons surface in the build error.
func HostSlimFunc() SlimFunc {
	bin := findSimslim()
	if bin == "" {
		return nil
	}
	return func(ctx context.Context, udid, profile string) error {
		args := []string{"on", udid, "--boot-timeout", "30m"}
		if profile != "" && profile != "default" {
			path, err := resolveSlimProfile(profile)
			if err != nil {
				return err
			}
			args = append(args, "--profile", path)
		}
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// HostSlimVerifyFunc returns a SlimVerifyFunc backed by `simslim verify`
// (exact profile-match check with drift listing, simslim >= v0.6.0), or
// nil when no simslim is installed or the installed binary predates the
// verify command — callers fall back to their own launchctl
// print-disabled parsing in that case. Support is probed once per
// process and cached.
func HostSlimVerifyFunc() SlimVerifyFunc {
	bin := findSimslim()
	if bin == "" || !simslimSupportsVerify(bin) {
		return nil
	}
	return func(ctx context.Context, udid, profile string) error {
		args := []string{"verify", udid}
		if profile != "" && profile != "default" {
			path, err := resolveSlimProfile(profile)
			if err != nil {
				return err
			}
			args = append(args, "--profile", path)
		}
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

var (
	verifyProbeOnce sync.Once
	verifyProbeOK   bool
)

// simslimSupportsVerify reports whether bin has the `verify` subcommand
// (added in simslim v0.6.0) by looking for it in `simslim help` output.
// Exit codes cannot distinguish support: `help verify` exits 0 on old
// binaries too, and bare `verify` fails either way (missing UDID vs
// unknown command). Probed once; the binary does not change mid-process.
// The probe is bounded so a wedged binary (e.g. a stalled network mount)
// degrades to "verify unsupported" instead of blocking daemon startup.
func simslimSupportsVerify(bin string) bool {
	verifyProbeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, "help").CombinedOutput()
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.Fields(line); len(f) > 0 && f[0] == "verify" {
				verifyProbeOK = true
				return
			}
		}
	})
	return verifyProbeOK
}

// HostSlimProfileCheck pre-validates a slim profile name against the
// host filesystem without running simslim; wired via SetSlimCheck so a
// typo'd profile fails as a bad request before a builder sim is created.
func HostSlimProfileCheck(profile string) error {
	if profile == "" || profile == "default" {
		return nil
	}
	_, err := resolveSlimProfile(profile)
	return err
}

func findSimslim() string {
	if p := os.Getenv("MANZANASD_SIMSLIM"); p != "" {
		// Validate the override like every other candidate; a stale path
		// must mean "unavailable", not a doomed build later.
		if isExecutableFile(p) {
			return p
		}
		return ""
	}
	home, err := os.UserHomeDir()
	if err == nil {
		for _, p := range []string{
			filepath.Join(home, "bin", "simslim"),
			filepath.Join(home, "simtest", "bin", "simslim"),
		} {
			if isExecutableFile(p) {
				return p
			}
		}
	}
	if p, err := exec.LookPath("simslim"); err == nil {
		return p
	}
	return ""
}

// isExecutableFile reports whether p is a regular file with an execute
// bit set — a directory of the right name must not pass for the binary.
func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// resolveSlimProfile maps a profile name to ~/qa/<name>.json (the
// fleet's deployed profile location). "default" means simslim's built-in
// default profile (handled by the caller); anything unresolvable is an
// error rather than a silent fallback. Names are bare identifiers, never
// paths: the value comes off the wire and must not address arbitrary
// host files.
func resolveSlimProfile(profile string) (string, error) {
	if !slimProfileNameRe.MatchString(profile) {
		return "", fmt.Errorf("slim profile %q: name must match %s", profile, slimProfileNameRe)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("slim profile %q: resolving home dir: %w", profile, err)
	}
	p := filepath.Join(home, "qa", profile+".json")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("slim profile %q: no such profile (looked for %s)", profile, p)
}
