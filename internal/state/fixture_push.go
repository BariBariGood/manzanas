package state

import (
	"context"
	"encoding/json"
	"fmt"
)

// pushFixture injects an APNs payload via `simctl push <udid> <bundle> -`.
// Payload: {"bundle_id": "com.example.app", "payload": {"aps": {...}}}.
// Requires a booted target.
type pushFixture struct{}

func (pushFixture) Name() string { return "push" }

func (pushFixture) Apply(ctx context.Context, run Runner, udid string, payload map[string]any) error {
	bundle, err := payloadString(payload, "bundle_id")
	if err != nil {
		return err
	}
	body, ok := payload["payload"].(map[string]any)
	if !ok {
		return fmt.Errorf("%w: \"payload\" must be an APNs JSON object", ErrBadFixture)
	}
	if _, ok := body["aps"]; !ok {
		return fmt.Errorf("%w: push payload must contain an \"aps\" key", ErrBadFixture)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = run.SimctlInput(ctx, raw, "push", udid, bundle, "-")
	return err
}
