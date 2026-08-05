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
// dispatch.
type KindRouter struct {
	reg registry.Registry
	sim Backend
	dev Backend

	mu    sync.RWMutex
	kinds map[string]proto.TargetKind
}

// NewKindRouter builds the per-kind dispatch layer.
func NewKindRouter(reg registry.Registry, sim, dev Backend) *KindRouter {
	return &KindRouter{reg: reg, sim: sim, dev: dev, kinds: map[string]proto.TargetKind{}}
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
		return k, nil
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
