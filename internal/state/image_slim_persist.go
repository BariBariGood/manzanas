package state

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Persistent daemon disabling. simslim's launchctl disables are keyed to
// the sim's UDID outside its data directory (iOS 18+ runtimes), so a
// stamped sim — which keeps its own UDID — boots with none of the golden
// image's disables. The helpers here capture the builder's disable set at
// build time, re-apply it to every stamped sim, and re-apply it after
// `simctl erase`, always via `launchctl disable system/<svc>` (persists
// across reboots; never SIGKILL — respawn churn is worse than the daemon).

// printDisabledServices returns the services `launchctl print-disabled
// system` reports as disabled on a booted sim.
func printDisabledServices(ctx context.Context, run Runner, udid string) (map[string]bool, error) {
	out, err := run.Simctl(ctx, "spawn", udid, "launchctl", "print-disabled", "system")
	if err != nil {
		return nil, fmt.Errorf("launchctl print-disabled on %s: %w", udid, err)
	}
	disabled := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		// Lines look like `"com.apple.apsd" => disabled` (or `=> true`).
		line = strings.TrimSpace(line)
		start := strings.IndexByte(line, '"')
		if start != 0 {
			continue
		}
		end := strings.IndexByte(line[1:], '"')
		if end < 0 {
			continue
		}
		svc := line[1 : 1+end]
		rest := strings.TrimSpace(line[2+end:])
		if strings.HasPrefix(rest, "=>") {
			val := strings.TrimSpace(strings.TrimPrefix(rest, "=>"))
			if val == "disabled" || val == "true" {
				disabled[svc] = true
			}
		}
	}
	return disabled, nil
}

// disableService persistently disables one service on a booted sim.
func disableService(ctx context.Context, run Runner, udid, svc string) error {
	if _, err := run.Simctl(ctx, "spawn", udid, "launchctl", "disable", "system/"+svc); err != nil {
		return fmt.Errorf("launchctl disable system/%s on %s: %w", svc, udid, err)
	}
	return nil
}

// missingDisables returns the subset of want not currently disabled.
func missingDisables(want []string, have map[string]bool) []string {
	var missing []string
	for _, svc := range want {
		if !have[svc] {
			missing = append(missing, svc)
		}
	}
	return missing
}

// ensureDisabled verifies every service in want is disabled on a booted
// sim, re-applies any that are missing, and verifies again. A service
// still enabled after re-apply is a loud failure, never a silent
// unslimmed sim.
func ensureDisabled(ctx context.Context, run Runner, udid string, want []string) error {
	have, err := printDisabledServices(ctx, run, udid)
	if err != nil {
		return err
	}
	missing := missingDisables(want, have)
	if len(missing) == 0 {
		return nil
	}
	for _, svc := range missing {
		if err := disableService(ctx, run, udid, svc); err != nil {
			return err
		}
	}
	have, err = printDisabledServices(ctx, run, udid)
	if err != nil {
		return err
	}
	if still := missingDisables(want, have); len(still) > 0 {
		return fmt.Errorf("sim %s: %d service(s) still enabled after re-applying disables (first: %s)", udid, len(still), still[0])
	}
	return nil
}

// runningServiceCount counts the running launchd services on a booted sim
// (lines of `launchctl list` whose PID column is numeric). Best-effort
// footprint metric; -1 when it cannot be measured.
func runningServiceCount(ctx context.Context, run Runner, udid string) int {
	out, err := run.Simctl(ctx, "spawn", udid, "launchctl", "list")
	if err != nil {
		return -1
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if _, err := strconv.Atoi(fields[0]); err == nil {
			count++
		}
	}
	return count
}

// slimRuntimeGuard refuses slim builds on iOS runtimes older than 18:
// simslim silently no-ops there (observed on iOS 17.2), which would
// archive an unslimmed image labelled slim. Accepts display names
// ("iOS 17.2") and CoreSimulator identifiers
// ("com.apple.CoreSimulator.SimRuntime.iOS-17-2"); versions that cannot
// be parsed pass — the post-slim zero-disable check in Build still
// catches a no-op slim.
func slimRuntimeGuard(runtime string) error {
	name := runtime
	if strings.HasPrefix(runtime, runtimeIDPrefix) {
		name = strings.ReplaceAll(strings.TrimPrefix(runtime, runtimeIDPrefix), "-", " ")
	}
	fields := strings.Fields(name)
	if len(fields) < 2 || fields[0] != "iOS" {
		return nil
	}
	major, err := strconv.Atoi(strings.SplitN(fields[1], ".", 2)[0])
	if err != nil {
		return nil
	}
	if major < 18 {
		return fmt.Errorf("%w: slim_profile requires iOS 18 or newer (simslim silently no-ops on %s)", ErrBadImageRequest, runtime)
	}
	return nil
}
