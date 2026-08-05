package state

import (
	"context"
	"fmt"
)

// privacyFixture drives `simctl privacy <udid> <action> <service> [bundle]`.
// Payload: {"action": "grant"|"revoke"|"reset", "service": "photos"|
// "camera"|"microphone"|"location"|"contacts"|"all"|..., "bundle_id":
// "com.example.app"} (bundle_id optional for reset).
type privacyFixture struct{}

func (privacyFixture) Name() string { return "privacy" }

func (privacyFixture) Apply(ctx context.Context, run Runner, udid string, payload map[string]any) error {
	action, err := payloadString(payload, "action")
	if err != nil {
		return err
	}
	if action != "grant" && action != "revoke" && action != "reset" {
		return fmt.Errorf("%w: action must be grant, revoke, or reset", ErrBadFixture)
	}
	service, err := payloadString(payload, "service")
	if err != nil {
		return err
	}
	args := []string{"privacy", udid, action, service}
	bundle, _ := payload["bundle_id"].(string)
	if bundle != "" {
		if err := flagSafe(bundle); err != nil {
			return fmt.Errorf("%w: \"bundle_id\": %v", ErrBadFixture, err)
		}
		args = append(args, bundle)
	} else if action != "reset" {
		return fmt.Errorf("%w: bundle_id is required for %s", ErrBadFixture, action)
	}
	_, err = run.Simctl(ctx, args...)
	return err
}
