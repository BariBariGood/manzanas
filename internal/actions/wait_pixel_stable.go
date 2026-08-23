package actions

import (
	"bytes"
	"context"
	"errors"
	"image"
	"time"

	_ "image/jpeg" // WDA builds may answer JPEG screenshots
	_ "image/png"

	"github.com/BariBariGood/manzanas/internal/imgutil"
)

// wait_for_stable: pixel-diff screen settle. Frames are captured through
// the backend's screenshot path, downscaled to quarter-resolution
// grayscale, and compared with a mean-absolute-diff; the screen counts as
// settled once consecutive comparisons stay under the threshold for the
// requested quiet window. Unlike wait_tree_stable this needs no a11y
// tree or OCR pass per poll, so it works identically on simulators, WDA
// devices, and mirror-backed devices.
const (
	// defaultPixelQuiet is how long the screen must hold still (all
	// frame-to-frame diffs under the threshold) to count as settled.
	defaultPixelQuiet = 500 * time.Millisecond
	// defaultPixelThreshold is the normalized mean-abs-diff [0,1] under
	// which two frames count as "the same screen". Quarter-res gray
	// box-averaging already suppresses single-pixel noise; 0.002 admits
	// antialiasing jitter while catching any visible content change.
	defaultPixelThreshold = 0.002
	// pixelDownscaleFactor shrinks captures before diffing: quarter
	// resolution keeps the comparison O(pixels/16) while remaining far
	// finer-grained than any real UI change.
	pixelDownscaleFactor = 4
)

// frameCapture captures one screen frame as encoded image bytes.
type frameCapture func(ctx context.Context) ([]byte, error)

// handleWaitPixelStable is the simulator wait_for_stable handler,
// polling AXe/simctl screenshots.
func handleWaitPixelStable(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	return pixelStable(ctx, p, func(ctx context.Context) ([]byte, error) {
		img, _, err := b.capturePNG(ctx, udid)
		return img, err
	})
}

// handleDeviceWaitPixelStable is the WDA wait_for_stable handler,
// polling WebDriverAgent screenshots.
func handleDeviceWaitPixelStable(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.wdaFor(udid)
	if err != nil {
		return nil, err
	}
	res, err := pixelStable(ctx, p, func(ctx context.Context) ([]byte, error) {
		img, err := c.Screenshot(ctx)
		if err != nil {
			// Transient transport blips (dropped tunnel, dead runner) are
			// "not yet": kick the supervisor and keep polling within the
			// budget, like the other device wait loops (observeOnce).
			if isTransientWDAError(err) {
				b.mu.RLock()
				k := b.kick[udid]
				b.mu.RUnlock()
				if k != nil {
					k()
				}
				return nil, errNotYet
			}
			return nil, b.wdaFail(udid, "wait_for_stable", err)
		}
		return img, nil
	})
	if err != nil {
		return nil, err
	}
	res["backend"] = "wda"
	return res, nil
}

// handleMirrorWaitPixelStable is the mirror wait_for_stable handler,
// polling mirrord window captures.
func handleMirrorWaitPixelStable(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.mirrorClient(udid)
	if err != nil {
		return nil, err
	}
	res, err := pixelStable(ctx, p, func(ctx context.Context) ([]byte, error) {
		scr, err := c.Screenshot(ctx)
		if err != nil {
			// Momentary capture hiccups (window re-compositing) are "not
			// yet"; blocked/no-window stay hard failures, as in
			// mirrorObserveOnce.
			if mirrorObserveTransient(err) {
				return nil, errNotYet
			}
			return nil, mirrorFail("wait_for_stable", err)
		}
		return scr.PNG, nil
	})
	if err != nil {
		return nil, err
	}
	// No fidelity:"ocr" here: the pixel wait involves no OCR.
	res["backend"] = "mirror"
	return res, nil
}

// pixelStable runs the shared wait_for_stable loop over a capture
// function. Payload: quiet_ms (default 500), timeout_ms (default 15000,
// max 120000), interval_ms (default 500, min 10), threshold (default
// 0.002, exclusive 0..1), mask ([]{x,y,w,h} capture-pixel rects excluded
// from the diff), require_stable (default false). Result:
// {stable, waited_ms, last_diff, samples} — a screen that never settles
// within the budget returns stable:false as an observation, not an
// error, unless require_stable is set (mirroring wait_tree_stable).
func pixelStable(ctx context.Context, p map[string]any, capture frameCapture) (map[string]any, error) {
	quiet, err := optDuration(p, "quiet_ms", defaultPixelQuiet)
	if err != nil {
		return nil, err
	}
	if quiet > maxWaitTimeout {
		quiet = maxWaitTimeout
	}
	timeout, interval, err := waitParams(p, defaultStableTimeout)
	if err != nil {
		return nil, err
	}
	threshold, err := pixelThreshold(p)
	if err != nil {
		return nil, err
	}
	masks, err := maskRects(p)
	if err != nil {
		return nil, err
	}
	requireStable, err := boolFlag(p, "require_stable", false)
	if err != nil {
		return nil, err
	}
	// The quiet window expressed as consecutive under-threshold
	// frame-to-frame comparisons at the polling interval.
	need := int((quiet + interval - 1) / interval)
	if need < 1 {
		need = 1
	}

	var prev *image.Gray
	var lastDiff float64
	compared, streak := 0, 0
	polls, elapsed, err := pollUntil(ctx, timeout, interval, func(ctx context.Context) error {
		frame, err := capture(ctx)
		if err != nil {
			// An unobserved gap (transient capture failure) cannot count
			// toward the quiet window, and the pre-outage frame is stale
			// evidence: restart the streak from the next capture.
			if errors.Is(err, errNotYet) {
				streak, prev = 0, nil
			}
			return err
		}
		img, _, err := image.Decode(bytes.NewReader(frame))
		if err != nil {
			return internal("wait_for_stable: capture is not a decodable image: %v", err)
		}
		g := imgutil.GrayDownscale(img, quarterDim(img))
		scaled := scaleMasks(masks, img.Bounds(), g.Bounds())
		if prev == nil {
			prev = g
			return errNotYet
		}
		d := imgutil.MeanAbsDiff(prev, g, scaled)
		prev = g
		lastDiff = d
		compared++
		if d < threshold {
			streak++
		} else {
			streak = 0
		}
		if streak >= need {
			return nil
		}
		return errNotYet
	})
	if err != nil {
		// The caller's own deadline (e.g. the batch wall-clock budget)
		// cutting the wait short is a real timeout, not an observation
		// that the screen is live: the wait never spent its budget.
		if ctx.Err() != nil {
			return nil, timeoutErr("wait_for_stable ended early after %s (%d poll(s)): %v", elapsed.Round(time.Millisecond), polls, ctx.Err())
		}
		if errors.Is(err, errNotYet) || errors.Is(err, context.DeadlineExceeded) {
			// No two frames were ever compared: the backend never produced
			// enough captures to observe the screen at all (dead runner,
			// mirroring hiccup on every poll). That is a poll failure, not
			// an observation that the screen is live — mirroring
			// wait_tree_stable's never-readable-tree guard.
			if compared == 0 {
				return nil, timeoutErr("wait_for_stable captured no comparable frames within the %s max wait (%d poll(s), %s elapsed)", timeout, polls, elapsed.Round(time.Millisecond))
			}
			if requireStable {
				return nil, timeoutErr("screen did not settle within the %s max wait (%d poll(s), %s elapsed, last diff %.5f)", timeout, polls, elapsed.Round(time.Millisecond), lastDiff)
			}
			return map[string]any{
				"stable":    false,
				"waited_ms": elapsed.Milliseconds(),
				"last_diff": lastDiff,
				"samples":   polls,
			}, nil
		}
		return nil, err
	}
	return map[string]any{
		"stable":    true,
		"waited_ms": elapsed.Milliseconds(),
		"last_diff": lastDiff,
		"samples":   polls,
	}, nil
}

// quarterDim returns the longer-edge cap that shrinks img to quarter
// resolution (at least 1 pixel).
func quarterDim(img image.Image) int {
	b := img.Bounds()
	long := b.Dx()
	if b.Dy() > long {
		long = b.Dy()
	}
	d := long / pixelDownscaleFactor
	if d < 1 {
		d = 1
	}
	return d
}

// pixelThreshold reads the optional threshold payload field (exclusive
// 0..1 normalized mean-abs-diff).
func pixelThreshold(p map[string]any) (float64, error) {
	v, ok := p["threshold"]
	if !ok {
		return defaultPixelThreshold, nil
	}
	n, err := toNum(v)
	if err != nil || n <= 0 || n >= 1 {
		return 0, badRequest("payload field %q must be a number between 0 and 1 (exclusive)", "threshold")
	}
	return n, nil
}

// maskRects reads the optional mask payload field: a list of {x,y,w,h}
// rectangles, in capture-image pixels, excluded from the diff (volatile
// regions such as clocks or spinners).
func maskRects(p map[string]any) ([]image.Rectangle, error) {
	v, ok := p["mask"]
	if !ok {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, badRequest("payload field %q must be a list of {x,y,w,h} rectangles", "mask")
	}
	rects := make([]image.Rectangle, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, badRequest("payload field %q must be a list of {x,y,w,h} rectangles", "mask")
		}
		var vals [4]float64
		for i, k := range [4]string{"x", "y", "w", "h"} {
			f, ok := m[k]
			if !ok {
				return nil, badRequest("mask rectangle is missing field %q", k)
			}
			n, err := toNum(f)
			if err != nil || n < 0 {
				return nil, badRequest("mask rectangle field %q must be a non-negative number", k)
			}
			vals[i] = n
		}
		if vals[2] <= 0 || vals[3] <= 0 {
			return nil, badRequest("mask rectangle must have positive w and h")
		}
		rects = append(rects, image.Rect(int(vals[0]), int(vals[1]), int(vals[0]+vals[2]), int(vals[1]+vals[3])))
	}
	return rects, nil
}

// scaleMasks maps capture-pixel mask rectangles into the downscaled
// frame's coordinate space, rounding outward so a mask never uncovers
// part of the volatile region it was meant to hide.
func scaleMasks(masks []image.Rectangle, src, dst image.Rectangle) []image.Rectangle {
	if len(masks) == 0 || src.Dx() <= 0 || src.Dy() <= 0 {
		return nil
	}
	sx := float64(dst.Dx()) / float64(src.Dx())
	sy := float64(dst.Dy()) / float64(src.Dy())
	out := make([]image.Rectangle, 0, len(masks))
	for _, m := range masks {
		out = append(out, image.Rect(
			int(float64(m.Min.X)*sx),
			int(float64(m.Min.Y)*sy),
			int(float64(m.Max.X)*sx+0.999999),
			int(float64(m.Max.Y)*sy+0.999999),
		))
	}
	return out
}
