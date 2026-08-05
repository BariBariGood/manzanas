package state

import "context"

// timezoneFixture sets the TZ environment variable inside the simulator's
// launchd via `simctl spawn <udid> launchctl setenv TZ <tz>`, affecting
// processes launched afterwards. Payload: {"tz": "America/Los_Angeles"}.
// Requires a booted target; best-effort (already-running processes keep
// their zone until relaunched).
type timezoneFixture struct{}

func (timezoneFixture) Name() string { return "timezone" }

func (timezoneFixture) Apply(ctx context.Context, run Runner, udid string, payload map[string]any) error {
	tz, err := payloadString(payload, "tz")
	if err != nil {
		return err
	}
	_, err = run.Simctl(ctx, "spawn", udid, "launchctl", "setenv", "TZ", tz)
	return err
}
