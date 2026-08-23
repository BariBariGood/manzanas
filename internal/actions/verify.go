package actions

import (
	"context"
	"image"
	"time"

	"github.com/BariBariGood/manzanas/internal/imgutil"
)

// Opt-in verified taps/swipes (payload {"verify": true}): the backend
// captures a downscaled grayscale frame before and after the input and
// compares them with a masked mean absolute pixel difference. The result
// gains `verified: true|false` plus the measured `pixel_delta` — a
// report, never a failure: a tap that legitimately changes nothing
// (tapping an already-selected tab) is `verified: false` with `ok:
// true`, and the caller decides what that means. Capture failures
// degrade to a `verify_error` field, keeping the action's own success
// authoritative.

// verifyKinds are the raw HID kinds accepting the verify payload flag.
var verifyKinds = map[string]bool{
	"tap":   true,
	"swipe": true,
}

const (
	// verifyMaxDim is the longer-edge cap the pre/post frames are
	// grayscale-downscaled to before diffing: small enough that two
	// captures add single-digit milliseconds, large enough that a
	// switch-sized control still moves the mean.
	verifyMaxDim = 128

	// defaultVerifyThreshold is the mean-absolute-difference (0..1) at
	// or above which the frames count as changed. Deliberately low: at
	// 128 px a toggle flip moves the mean by ~0.002, so anything much
	// higher only detects full-screen transitions.
	defaultVerifyThreshold = 0.001

	// defaultVerifySettle is how long the post-frame capture waits for
	// the UI reaction (tap animation, screen transition) to render.
	defaultVerifySettle = 300 * time.Millisecond

	// maxVerifySettle clamps verify_settle_ms: the settle waits for one
	// UI reaction to render, so anything longer only pins the action
	// slot (matching the clamps on timeout_ms and the mirror durations).
	maxVerifySettle = 10 * time.Second
)

// verifySpec is the parsed verify payload.
type verifySpec struct {
	enabled   bool
	threshold float64
	settle    time.Duration
	// masks are volatile regions to exclude from the diff, in native
	// capture coordinates (rescaled with the frame before comparing).
	masks []Frame
}

// verifySpecFromPayload reads the optional verify fields: `verify`
// (bool), `verify_threshold` (number in (0, 1]), `verify_settle_ms`
// (positive ms), `verify_masks` (array of {x,y,w,h} rects in native
// capture coordinates).
func verifySpecFromPayload(p map[string]any) (verifySpec, error) {
	s := verifySpec{threshold: defaultVerifyThreshold, settle: defaultVerifySettle}
	enabled, err := boolFlag(p, "verify", false)
	if err != nil {
		return s, err
	}
	s.enabled = enabled
	if v, ok := p["verify_threshold"]; ok {
		n, err := toNum(v)
		if err != nil || n <= 0 || n > 1 {
			return s, badRequest("payload field %q must be a number in (0, 1]", "verify_threshold")
		}
		s.threshold = n
	}
	settle, err := optDuration(p, "verify_settle_ms", defaultVerifySettle)
	if err != nil {
		return s, err
	}
	if settle > maxVerifySettle {
		settle = maxVerifySettle
	}
	s.settle = settle
	if v, ok := p["verify_masks"]; ok {
		arr, ok := v.([]any)
		if !ok {
			return s, badRequest("payload field %q must be an array of {x,y,w,h} rects", "verify_masks")
		}
		for _, e := range arr {
			m, ok := e.(map[string]any)
			if !ok {
				return s, badRequest("payload field %q must be an array of {x,y,w,h} rects", "verify_masks")
			}
			var f Frame
			for _, kv := range []struct {
				key string
				dst *float64
			}{{"x", &f.X}, {"y", &f.Y}, {"w", &f.W}, {"h", &f.H}} {
				n, err := toNum(m[kv.key])
				if err != nil || n < 0 {
					return s, badRequest("verify_masks rects require non-negative numeric %q", kv.key)
				}
				*kv.dst = n
			}
			s.masks = append(s.masks, f)
		}
	}
	if !s.enabled {
		if _, ok := p["verify_threshold"]; ok {
			return s, badRequest("payload field %q requires %q", "verify_threshold", "verify: true")
		}
		if _, ok := p["verify_masks"]; ok {
			return s, badRequest("payload field %q requires %q", "verify_masks", "verify: true")
		}
		if _, ok := p["verify_settle_ms"]; ok {
			return s, badRequest("payload field %q requires %q", "verify_settle_ms", "verify: true")
		}
	}
	return s, nil
}

// verifyPrecheck validates the wrapped HID payload's coordinate fields
// before the pre-frame capture: a capture is real work (mirror captures
// drive the exclusive GUI helper), so a request the handler would
// reject must not cost one.
func verifyPrecheck(kind string, p map[string]any) error {
	var keys []string
	switch kind {
	case "tap":
		keys = []string{"x", "y"}
	case "swipe":
		keys = []string{"start_x", "start_y", "end_x", "end_y"}
	}
	for _, k := range keys {
		if _, err := coordField(p, k); err != nil {
			return err
		}
	}
	return nil
}

// runVerified wraps one action dispatch with the pre/post capture. The
// action's own result/error stays authoritative: verification never
// fails a succeeded action, and a capture failure degrades to
// verify_error.
func runVerified(ctx context.Context, spec verifySpec, capture frameCaptureFunc,
	do func() (map[string]any, error)) (map[string]any, error) {
	pre, _, preErr := capture(ctx)
	res, err := do()
	if err != nil {
		return nil, err
	}
	if res == nil {
		res = map[string]any{}
	}
	if preErr != nil {
		res["verify_error"] = "pre-frame capture failed: " + preErr.Error()
		return res, nil
	}
	select {
	case <-ctx.Done():
		res["verify_error"] = "canceled before the post-frame capture: " + ctx.Err().Error()
		return res, nil
	case <-time.After(spec.settle):
	}
	post, _, postErr := capture(ctx)
	if postErr != nil {
		res["verify_error"] = "post-frame capture failed: " + postErr.Error()
		return res, nil
	}
	delta := verifyDelta(pre, post, spec.masks)
	res["verified"] = delta >= spec.threshold
	res["pixel_delta"] = delta
	res["pixel_threshold"] = spec.threshold
	return res, nil
}

// verifyDelta downscales both frames and returns their masked mean
// absolute pixel difference. Masks are given in the pre frame's native
// coordinates and rescaled onto the downscaled grid.
func verifyDelta(pre, post image.Image, masks []Frame) float64 {
	a := imgutil.GrayDownscale(pre, verifyMaxDim)
	b := imgutil.GrayDownscale(post, verifyMaxDim)
	var rects []image.Rectangle
	if len(masks) > 0 {
		ow := pre.Bounds().Dx()
		oh := pre.Bounds().Dy()
		dw := a.Bounds().Dx()
		dh := a.Bounds().Dy()
		if ow > 0 && oh > 0 {
			sx := float64(dw) / float64(ow)
			sy := float64(dh) / float64(oh)
			for _, m := range masks {
				rects = append(rects, image.Rect(
					int(m.X*sx), int(m.Y*sy),
					int((m.X+m.W)*sx+0.5)+1, int((m.Y+m.H)*sy+0.5)+1))
			}
		}
	}
	return verifyRound(imgutil.MeanAbsDiff(a, b, rects))
}

// verifyRound keeps pixel_delta readable on the wire without hiding
// small-but-real deltas near the default threshold.
func verifyRound(v float64) float64 {
	const scale = 1e6
	return float64(int64(v*scale+0.5)) / scale
}
