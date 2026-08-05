package state

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Fixture applies one kind of deterministic environment mutation to a
// simulator. Payloads are opaque JSON maps; each implementation owns and
// documents its own schema (see docs/state.md).
type Fixture interface {
	// Name is the wire name of the fixture (e.g. "statusbar").
	Name() string
	// Apply applies the fixture to the target via the runner.
	Apply(ctx context.Context, run Runner, udid string, payload map[string]any) error
}

// builtinFixtures wires every fixture implementation into the engine.
func builtinFixtures() map[string]Fixture {
	all := []Fixture{
		statusBarFixture{},
		privacyFixture{},
		pushFixture{},
		localeFixture{},
		timezoneFixture{},
		urlFixture{},
	}
	m := make(map[string]Fixture, len(all))
	for _, f := range all {
		m[f.Name()] = f
	}
	return m
}

func fixtureNames(m map[string]Fixture) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// payloadString extracts a required string field from a fixture payload.
func payloadString(payload map[string]any, key string) (string, error) {
	v, ok := payload[key].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("%w: %q must be a non-empty string", ErrBadFixture, key)
	}
	if err := flagSafe(v); err != nil {
		return "", fmt.Errorf("%w: %q: %v", ErrBadFixture, key, err)
	}
	return v, nil
}

// flagSafe rejects payload values that would be parsed by simctl as a
// command-line flag instead of data (argument injection).
func flagSafe(v string) error {
	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("value %q must not start with '-'", v)
	}
	return nil
}
