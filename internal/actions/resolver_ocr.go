package actions

import (
	"context"
	"time"
)

// ocrResolver is rung 3: OCR text match over the iPhone Mirroring
// window capture. It registers only where an OCR source exists
// (mirror-backed devices); WDA-only devices have no OCR source, so
// their ladders never include it.
type ocrResolver struct {
	b *DeviceBackend
}

func (r *ocrResolver) name() string          { return "ocr" }
func (r *ocrResolver) budget() time.Duration { return ocrRungBudget }

func (r *ocrResolver) resolve(ctx context.Context, udid string, m elementMatcher, refresh bool) (*Node, observation, error) {
	obs, err := r.b.mirrorObserveOnce(ctx, udid)
	if err != nil {
		return nil, observation{}, err
	}
	return matchIn(obs, m)
}
