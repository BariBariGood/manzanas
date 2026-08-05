package registry

import (
	"context"
	"errors"
	"log/slog"

	"github.com/BariBariGood/manzanas/proto"
)

// MergedRegistry presents several registries (e.g. simulators via simctl
// plus physical devices via devicectl) behind one Registry. List
// concatenates in construction order; per-UDID operations route to the
// first registry that knows the UDID.
type MergedRegistry struct {
	regs []Registry
}

// Merge combines registries. Order matters: List concatenates in order and
// per-UDID lookups probe in order.
func Merge(regs ...Registry) *MergedRegistry {
	return &MergedRegistry{regs: regs}
}

// List degrades gracefully: a sub-registry failure (e.g. a devicectl
// timeout while CoreDevice is unhealthy) must not hide the targets of the
// registries that succeeded — otherwise enabling devices could take the
// simulator fleet down with it. Only when every registry fails does List
// return an error.
func (m *MergedRegistry) List(ctx context.Context) ([]proto.Target, error) {
	var out []proto.Target
	var errs []error
	for _, r := range m.regs {
		ts, err := r.List(ctx)
		if err != nil {
			slog.Warn("merged registry: sub-registry list failed", "err", err)
			errs = append(errs, err)
			continue
		}
		out = append(out, ts...)
	}
	if len(errs) == len(m.regs) {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// owner returns the first registry that knows udid, along with the target
// it already resolved (so callers need not re-Get: both sub-registries
// shell out per lookup). Like List, it degrades gracefully: a failing
// sub-registry is skipped so a transient simctl hiccup cannot break
// device lookups (and vice versa); its error only surfaces when no
// registry claims the UDID.
func (m *MergedRegistry) owner(ctx context.Context, udid string) (Registry, proto.Target, error) {
	var errs []error
	for _, r := range m.regs {
		t, err := r.Get(ctx, udid)
		if err == nil {
			return r, t, nil
		}
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			errs = append(errs, err)
		}
	}
	// The typed NotFoundError means every registry answered cleanly:
	// callers key destructive decisions off it (404 mapping, pool
	// pruning, simulator-backend fallback). When a sub-registry failed,
	// the target's existence is indeterminate — a device could be masked
	// behind an unhealthy devicectl — so only the failures are returned.
	if len(errs) > 0 {
		return nil, proto.Target{}, errors.Join(errs...)
	}
	return nil, proto.Target{}, &NotFoundError{UDID: udid}
}

func (m *MergedRegistry) Get(ctx context.Context, udid string) (proto.Target, error) {
	_, t, err := m.owner(ctx, udid)
	return t, err
}

func (m *MergedRegistry) Boot(ctx context.Context, udid string) error {
	r, _, err := m.owner(ctx, udid)
	if err != nil {
		return err
	}
	return r.Boot(ctx, udid)
}

func (m *MergedRegistry) Shutdown(ctx context.Context, udid string) error {
	r, _, err := m.owner(ctx, udid)
	if err != nil {
		return err
	}
	return r.Shutdown(ctx, udid)
}

func (m *MergedRegistry) Health(ctx context.Context, udid string) (proto.TargetState, error) {
	// The target owner already resolved carries a fresh state (both
	// sub-registries derive Health from the same listing), so re-asking
	// would just repeat the shell-out.
	_, t, err := m.owner(ctx, udid)
	if err != nil {
		return proto.StateUnknown, err
	}
	return t.State, nil
}
