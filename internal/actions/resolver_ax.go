package actions

import (
	"context"
	"time"
)

// simAXResolver is the simulator's rung 1: one a11y tree poll through
// the AXe backend (warm helper or cold CLI).
type simAXResolver struct {
	b *AXeBackend
}

func (r *simAXResolver) name() string          { return resolverAX }
func (r *simAXResolver) budget() time.Duration { return axRungBudget }

func (r *simAXResolver) resolve(ctx context.Context, udid string, m elementMatcher, refresh bool) (*Node, observation, error) {
	obs, err := r.b.observeOnce(ctx, udid, refresh)
	if err != nil {
		return nil, observation{}, err
	}
	return matchIn(obs, m)
}

// elementResolvers implements resolverProvider: simulators resolve
// through the element-map fast path (when the store is configured),
// then the a11y rung, falling back to the saved-template rung when a
// template library is configured (no OCR source on simulators).
func (b *AXeBackend) elementResolvers(udid string) resolverLadder {
	var rungs []resolver
	if b.emap.enabled() {
		rungs = append(rungs, &mapResolver{em: b.emap})
	}
	rungs = append(rungs, &simAXResolver{b})
	if b.templates != nil {
		rungs = append(rungs, &templateResolver{
			store:   b.templates,
			capture: b.captureFrame,
			space:   b.tapSpace,
			bundle:  b.launchedBundle,
		})
	}
	return resolverLadder{rungs: rungs}
}

// captureEvidence implements evidenceCapturer for escalation bundles.
func (b *AXeBackend) captureEvidence(ctx context.Context, udid string) ([]byte, error) {
	png, _, err := b.capturePNG(ctx, udid)
	return png, err
}
