package actions

import (
	"context"
)

// elementResolvers implements resolverProvider for physical devices.
// The element-map fast path (when the store is configured) runs first
// on both variants. Mirror-backed devices have no accessibility tree,
// so they resolve through OCR then the saved-template rung; WDA-backed
// devices resolve through the a11y rung (guarded by the per-lease,
// per-bundle circuit breaker) then templates. The template rung
// registers only when a template library is configured.
func (b *DeviceBackend) elementResolvers(udid string) resolverLadder {
	var rungs []resolver
	if b.emap.enabled() {
		rungs = append(rungs, &mapResolver{em: b.emap})
	}
	if b.mirrorFor(udid) != nil {
		rungs = append(rungs, &ocrResolver{b})
		if b.templates != nil {
			rungs = append(rungs, b.templateResolver())
		}
		return resolverLadder{rungs: rungs}
	}
	rungs = append(rungs, &wdaAXResolver{b})
	if b.templates != nil {
		rungs = append(rungs, b.templateResolver())
	}
	return resolverLadder{
		rungs:   rungs,
		breaker: b.axBreak,
		bundle:  b.launchedBundle(udid),
	}
}

func (b *DeviceBackend) templateResolver() *templateResolver {
	return &templateResolver{
		store:   b.templates,
		capture: b.captureFrame,
		space:   b.tapSpace,
		bundle:  b.launchedBundle,
	}
}

// noteLaunchedBundle records the bundle last launched on a device, the
// key the AX circuit breaker scopes its per-bundle state with.
func (b *DeviceBackend) noteLaunchedBundle(udid, bundleID string) {
	b.mu.Lock()
	b.bundles[udid] = bundleID
	b.mu.Unlock()
}

func (b *DeviceBackend) launchedBundle(udid string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.bundles[udid]
}

// captureEvidence implements evidenceCapturer for escalation bundles:
// the mirror window capture when the device is mirror-backed, the WDA
// screenshot otherwise.
func (b *DeviceBackend) captureEvidence(ctx context.Context, udid string) ([]byte, error) {
	if m := b.mirrorFor(udid); m != nil {
		scr, err := m.Screenshot(ctx)
		if err != nil {
			return nil, err
		}
		return scr.PNG, nil
	}
	c, err := b.wdaFor(udid)
	if err != nil {
		return nil, err
	}
	return c.Screenshot(ctx)
}
