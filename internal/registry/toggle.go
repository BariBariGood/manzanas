package registry

import (
	"context"
	"sync/atomic"

	"github.com/BariBariGood/manzanas/proto"
)

// ToggleRegistry wraps a base registry with an extra one (physical devices
// via devicectl) that can be switched on and off at runtime, so a daemon
// started without --devices can attach a phone later without restarting.
// While off it behaves exactly like the base registry.
type ToggleRegistry struct {
	base   Registry
	merged Registry
	on     atomic.Bool
}

// NewToggle wraps base with extra behind a runtime switch (initially off).
func NewToggle(base, extra Registry) *ToggleRegistry {
	return &ToggleRegistry{base: base, merged: Merge(base, extra)}
}

// SetEnabled switches the extra registry on or off.
func (t *ToggleRegistry) SetEnabled(on bool) { t.on.Store(on) }

// Enabled reports whether the extra registry is on.
func (t *ToggleRegistry) Enabled() bool { return t.on.Load() }

func (t *ToggleRegistry) active() Registry {
	if t.on.Load() {
		return t.merged
	}
	return t.base
}

// List implements Registry.
func (t *ToggleRegistry) List(ctx context.Context) ([]proto.Target, error) {
	return t.active().List(ctx)
}

// Get implements Registry.
func (t *ToggleRegistry) Get(ctx context.Context, udid string) (proto.Target, error) {
	return t.active().Get(ctx, udid)
}

// Boot implements Registry.
func (t *ToggleRegistry) Boot(ctx context.Context, udid string) error {
	return t.active().Boot(ctx, udid)
}

// Shutdown implements Registry.
func (t *ToggleRegistry) Shutdown(ctx context.Context, udid string) error {
	return t.active().Shutdown(ctx, udid)
}

// Health implements Registry.
func (t *ToggleRegistry) Health(ctx context.Context, udid string) (proto.TargetState, error) {
	return t.active().Health(ctx, udid)
}
