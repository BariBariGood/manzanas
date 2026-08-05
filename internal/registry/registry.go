// Package registry enumerates and manages leasable targets (simulators in
// v0.1; physical devices in v0.2 behind the same interface).
package registry

import (
	"context"

	"github.com/BariBariGood/manzanas/proto"
)

// Registry abstracts the source of targets so that physical devices (v0.2)
// and mock targets (tests, non-macOS dev) plug in behind one interface.
type Registry interface {
	// List returns all known targets with fresh state.
	List(ctx context.Context) ([]proto.Target, error)
	// Get returns one target by UDID, or proto.ErrNotFound.
	Get(ctx context.Context, udid string) (proto.Target, error)
	// Boot starts the target asynchronously; callers poll Get for state.
	Boot(ctx context.Context, udid string) error
	// Shutdown stops the target.
	Shutdown(ctx context.Context, udid string) error
	// Health re-polls the target's state and reports whether it is usable.
	Health(ctx context.Context, udid string) (proto.TargetState, error)
}

// NotFoundError is returned when a UDID is unknown to the registry.
type NotFoundError struct{ UDID string }

func (e *NotFoundError) Error() string { return "target not found: " + e.UDID }
