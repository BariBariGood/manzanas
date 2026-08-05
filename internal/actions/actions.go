// Package actions defines the interface contract for the HID/a11y action
// backend. The implementation is owned by the "actions" slice; the
// foundation only defines the contract and routes opaque payloads to it.
package actions

import (
	"context"

	"github.com/BariBariGood/manzanas/proto"
)

// Backend executes actions (tap/swipe/type/observe/screenshot/...) on a
// target. Payloads are opaque to the core: the backend owns their schema so
// slices don't collide on wire types.
type Backend interface {
	// Dispatch runs one action on the target identified by udid. The lease
	// has already been validated by the server.
	Dispatch(ctx context.Context, udid string, req proto.ActionRequest) (proto.ActionResult, error)
}
