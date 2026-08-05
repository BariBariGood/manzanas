package state

import "context"

// urlFixture opens a URL (deep link or web) via `simctl openurl`.
// Payload: {"url": "https://..." | "myapp://..."}. Requires a booted target.
type urlFixture struct{}

func (urlFixture) Name() string { return "url" }

func (urlFixture) Apply(ctx context.Context, run Runner, udid string, payload map[string]any) error {
	u, err := payloadString(payload, "url")
	if err != nil {
		return err
	}
	_, err = run.Simctl(ctx, "openurl", udid, u)
	return err
}
