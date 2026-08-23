package registry

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/BariBariGood/manzanas/proto"
)

// MergedRegistry presents several registries (e.g. simulators via simctl
// plus physical devices via devicectl plus mirror-backed devices) behind
// one Registry. List concatenates in construction order, deduplicating
// targets that several sub-registries know (a mirror-backed phone that
// devicectl also enumerates); per-UDID operations route to the first
// registry that knows the UDID.
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
	fallible := 0
	for _, r := range m.regs {
		if _, ok := r.(supplemental); !ok {
			fallible++
		}
		ts, err := r.List(ctx)
		if err != nil {
			slog.Warn("merged registry: sub-registry list failed", "err", err)
			errs = append(errs, err)
			continue
		}
		out = append(out, ts...)
	}
	if fallible > 0 && len(errs) == fallible {
		return nil, errors.Join(errs...)
	}
	return dedupTargets(out), nil
}

// supplemental marks a sub-registry whose List never fails (e.g. the
// mirror registry, where probe errors are liveness rather than
// enumeration failures): its success must not stop List from surfacing a
// total outage of every fallible sub-registry.
type supplemental interface{ SupplementalRegistry() }

// dedupTargets collapses duplicate UDIDs across sub-registries into one
// target per UDID, overlaying later duplicates onto the first occurrence.
func dedupTargets(ts []proto.Target) []proto.Target {
	idx := make(map[string]int, len(ts))
	out := make([]proto.Target, 0, len(ts))
	for _, t := range ts {
		if i, ok := idx[t.UDID]; ok {
			out[i] = overlayTarget(out[i], t)
			continue
		}
		idx[t.UDID] = len(out)
		out = append(out, t)
	}
	return out
}

// overlayTarget merges two targets for the same UDID. When one side is a
// mirror entry (label "mirror": liveness derived from the mirrord
// helper), the other side's metadata wins (devicectl knows the real
// name/model/runtime) while the mirror side supplies liveness (state) and
// labels; the metadata side's "disconnected" (CoreDevice tunnel down)
// becomes "no-tunnel" — the mirror is usable without a tunnel, but
// devicectl-routed app-lifecycle actions are not — and the mirror side's
// "no-devicectl" is dropped because devicectl clearly knows the UDID.
// Without a mirror side, the first occurrence wins and labels are
// unioned.
func overlayTarget(base, extra proto.Target) proto.Target {
	baseMirror := slices.Contains(base.Labels, MirrorLabel)
	extraMirror := slices.Contains(extra.Labels, MirrorLabel)
	if baseMirror && !extraMirror {
		return overlayTarget(extra, base)
	}
	out := base
	if !extraMirror {
		out.Labels = unionLabels(base.Labels, extra.Labels)
		if out.State == proto.StateUnknown {
			out.State = extra.State
		}
		return out
	}
	// extra is the mirror entry: metadata from base, liveness from extra.
	if out.Name == "" {
		out.Name = extra.Name
	}
	out.State = extra.State
	labels := make([]string, 0, len(base.Labels)+len(extra.Labels))
	for _, l := range base.Labels {
		if l == DisconnectedLabel {
			l = NoTunnelLabel
		}
		labels = append(labels, l)
	}
	for _, l := range extra.Labels {
		if l == NoDevicectlLabel {
			continue
		}
		labels = append(labels, l)
	}
	out.Labels = unionLabels(labels, nil)
	return out
}

// unionLabels concatenates label sets preserving order and dropping
// duplicates.
func unionLabels(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, l := range append(append([]string{}, a...), b...) {
		if seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

// owner returns the first registry that knows udid, along with the target
// it already resolved (so callers need not re-Get: both sub-registries
// shell out per lookup). Like List, it degrades gracefully: a failing
// sub-registry is skipped so a transient simctl hiccup cannot break
// device lookups (and vice versa); its error only surfaces when no
// registry claims the UDID.
func (m *MergedRegistry) owner(ctx context.Context, udid string) (Registry, proto.Target, error) {
	var errs []error
	for i, r := range m.regs {
		t, err := r.Get(ctx, udid)
		if err == nil {
			// Devices may be known to several sub-registries (devicectl
			// metadata + mirror liveness): overlay the rest, best-effort.
			// Simulators stay single-sourced, so their lookups keep the
			// early exit and never pay for a devicectl shell-out.
			if t.Kind == proto.TargetDevice {
				for _, r2 := range m.regs[i+1:] {
					if t2, err2 := r2.Get(ctx, udid); err2 == nil {
						t = overlayTarget(t, t2)
					}
				}
			}
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
