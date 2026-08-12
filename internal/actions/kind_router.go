package actions

import (
	"context"
	"errors"
	"sync"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// KindRouter implements Backend by looking up the target's kind in the
// registry and dispatching to the simulator backend (AXe/warm) or the
// physical-device backend. Kinds are cached per UDID: a target never
// changes kind, and actions must not pay a registry enumeration per
// dispatch. The cache must be invalidated (InvalidateKinds) whenever
// the registry's view can change, e.g. the runtime device toggle.
type KindRouter struct {
	reg registry.Registry
	sim Backend
	dev Backend
	// devicesOn short-circuits the whole lookup while physical devices
	// are off: every target is a simulator then, and a registry hiccup
	// (simctl shelling out on a busy host) must not fail actions on
	// deployments that never enabled devices.
	devicesOn func() bool

	mu    sync.RWMutex
	kinds map[string]proto.TargetKind
}

// NewKindRouter builds the per-kind dispatch layer. devicesOn may be
// nil (always consult the registry).
func NewKindRouter(reg registry.Registry, sim, dev Backend, devicesOn func() bool) *KindRouter {
	return &KindRouter{reg: reg, sim: sim, dev: dev, devicesOn: devicesOn, kinds: map[string]proto.TargetKind{}}
}

// kindOf resolves (and caches) the target's kind. A clean not-found
// defaults to the simulator backend, preserving pre-router behavior for
// targets the registry cannot see (the server has already validated the
// lease). Any other lookup failure is surfaced rather than guessed at:
// the merged registry only reports not-found when every sub-registry
// answered, so a failure could be masking a device.
func (r *KindRouter) kindOf(ctx context.Context, udid string) (proto.TargetKind, error) {
	r.mu.RLock()
	k, ok := r.kinds[udid]
	r.mu.RUnlock()
	if ok {
		// A UDID already known to be a device stays one even while the
		// runtime toggle is off: an in-flight lease's actions must reach
		// the device backend's clear "unavailable" instead of failing
		// with confusing simulator-tooling errors.
		return k, nil
	}
	if r.devicesOn != nil && !r.devicesOn() {
		return proto.TargetSimulator, nil
	}
	t, err := r.reg.Get(ctx, udid)
	if err != nil {
		var nf *registry.NotFoundError
		if errors.As(err, &nf) {
			return proto.TargetSimulator, nil
		}
		return "", err
	}
	r.mu.Lock()
	r.kinds[udid] = t.Kind
	r.mu.Unlock()
	return t.Kind, nil
}

// InvalidateKinds drops the per-UDID kind cache so the next dispatch
// re-resolves against the registry. Device entries are kept: a target
// never changes kind, and keeping them is what routes an in-flight
// device lease to the device backend after a runtime toggle-off.
func (r *KindRouter) InvalidateKinds() {
	r.mu.Lock()
	kept := map[string]proto.TargetKind{}
	for u, k := range r.kinds {
		if k == proto.TargetDevice {
			kept[u] = k
		}
	}
	r.kinds = kept
	r.mu.Unlock()
}

// SeedDeviceKinds records udids as physical devices ahead of any
// dispatch, so a lease granted before the target's first action still
// routes to the device backend after a runtime toggle-off (the cache is
// otherwise only populated by a successful dispatch-time lookup).
func (r *KindRouter) SeedDeviceKinds(udids []string) {
	r.mu.Lock()
	for _, u := range udids {
		r.kinds[u] = proto.TargetDevice
	}
	r.mu.Unlock()
}

// Dispatch implements Backend.
func (r *KindRouter) Dispatch(ctx context.Context, udid string, req proto.ActionRequest) (proto.ActionResult, error) {
	k, err := r.kindOf(ctx, udid)
	if err != nil {
		return proto.ActionResult{}, unavailable("cannot resolve kind of target %s: %v", udid, err)
	}
	if k == proto.TargetDevice {
		return r.dev.Dispatch(ctx, udid, req)
	}
	return r.sim.Dispatch(ctx, udid, req)
}
