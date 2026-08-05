package state

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// runtimeIDPrefix marks a runtime value that is already a CoreSimulator
// identifier and needs no resolution.
const runtimeIDPrefix = "com.apple.CoreSimulator.SimRuntime."

// resolveRuntime maps a runtime display name ("iOS 26.5" — the form the
// registry labels use) to its CoreSimulator identifier via
// `simctl list runtimes`, because `simctl create` only accepts
// identifiers. Identifiers pass through untouched; an unknown display
// name is the client's fault and answers bad_request with the installed
// runtimes listed.
func resolveRuntime(ctx context.Context, run Runner, runtime string) (string, error) {
	if strings.HasPrefix(runtime, runtimeIDPrefix) {
		return runtime, nil
	}
	out, err := run.Simctl(ctx, "list", "runtimes", "--json")
	if err != nil {
		return "", err
	}
	var parsed struct {
		Runtimes []struct {
			Name        string `json:"name"`
			Identifier  string `json:"identifier"`
			IsAvailable bool   `json:"isAvailable"`
		} `json:"runtimes"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", fmt.Errorf("parse simctl list runtimes: %w", err)
	}
	var installed []string
	for _, rt := range parsed.Runtimes {
		if !rt.IsAvailable {
			continue
		}
		if rt.Name == runtime {
			return rt.Identifier, nil
		}
		installed = append(installed, rt.Name)
	}
	return "", fmt.Errorf("%w: unknown runtime %q (installed: %s)",
		ErrBadImageRequest, runtime, strings.Join(installed, ", "))
}

// simctlCreateErr classifies a simctl create failure: an unknown device
// type or runtime is the client's input, not a daemon fault.
func simctlCreateErr(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "Invalid runtime") || strings.Contains(msg, "Invalid device type") {
		return fmt.Errorf("%w: %v", ErrBadImageRequest, err)
	}
	return err
}
