package actions

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
	"github.com/BariBariGood/manzanas/internal/imgutil"
	"github.com/BariBariGood/manzanas/internal/templates"
)

// tap_template: match a pixel template (inline base64 or a stored
// save_template entry) against the current screen capture and tap the
// match centre — the matching half of the template library (#194).
// It shares assert_template's payload/matching core; what it adds is
// the tap, an optional search rect, and the capture-pixel → tap-space
// conversion (mirror taps are already capture pixels; simulator and
// WDA taps are points, mapped via the observed viewport width).

// tapFunc delivers one tap in the backend's tap coordinate space.
type tapFunc func(ctx context.Context, x, y float64) error

// tapSpaceFunc returns the capture-pixels-per-tap-unit scale for a
// capture imgW pixels wide (1 when the capture and tap spaces already
// coincide, as on the mirror).
type tapSpaceFunc func(ctx context.Context, imgW int) (float64, error)

// tapTemplateSpec is the parsed tap_template payload: assert_template's
// fields plus an optional search rect (capture pixels).
type tapTemplateSpec struct {
	assertTemplateSpec
	rect *templates.Rect
}

func tapTemplateSpecFromPayload(p map[string]any, store *templates.Store) (tapTemplateSpec, error) {
	base, err := assertTemplateSpecFromPayload(p, store)
	if err != nil {
		return tapTemplateSpec{}, err
	}
	spec := tapTemplateSpec{assertTemplateSpec: base}
	rectVal, ok := p["rect"]
	if !ok || rectVal == nil {
		return spec, nil
	}
	m, ok := rectVal.(map[string]any)
	if !ok {
		return spec, badRequest("payload field %q must be a {x,y,w,h} object (pixels in the capture image)", "rect")
	}
	var vals [4]int
	for i, k := range []string{"x", "y", "w", "h"} {
		n, err := toNum(m[k])
		if err != nil || n != float64(int(n)) {
			return spec, badRequest("rect field %q must be an integer number of pixels", k)
		}
		vals[i] = int(n)
	}
	r := templates.Rect{X: vals[0], Y: vals[1], W: vals[2], H: vals[3]}
	if r.X < 0 || r.Y < 0 || r.W <= 0 || r.H <= 0 {
		return spec, badRequest("rect x/y must be >= 0 and w/h > 0")
	}
	spec.rect = &r
	return spec, nil
}

// searchRegion restricts the matched frame to the spec's rect (when
// present), keeping the sub-image's coordinates in full-capture space
// so match positions need no rebasing.
func (s tapTemplateSpec) searchRegion(gray *image.Gray) (*image.Gray, error) {
	if s.rect == nil {
		return gray, nil
	}
	b := gray.Bounds()
	if s.rect.X+s.rect.W > b.Dx() || s.rect.Y+s.rect.H > b.Dy() {
		return nil, badRequest("rect {x:%d,y:%d,w:%d,h:%d} exceeds the capture size %dx%d",
			s.rect.X, s.rect.Y, s.rect.W, s.rect.H, b.Dx(), b.Dy())
	}
	return gray.SubImage(image.Rect(s.rect.X, s.rect.Y, s.rect.X+s.rect.W, s.rect.Y+s.rect.H)).(*image.Gray), nil
}

// tapTemplate polls screen captures until the template matches at or
// above the threshold, then taps the match centre. A budget that
// expires without a match is a timeout error carrying the best score
// seen, so the caller knows how close the screen came.
func tapTemplate(ctx context.Context, p map[string]any, store *templates.Store, capture frameCaptureFunc, space tapSpaceFunc, tap tapFunc) (map[string]any, error) {
	spec, err := tapTemplateSpecFromPayload(p, store)
	if err != nil {
		return nil, err
	}
	timeout, interval, err := waitParams(p, defaultAssertTimeout)
	if err != nil {
		return nil, err
	}
	tmpl := spec.tmpl
	var mx, my, imgW int
	var best float64
	var class string
	observed := false
	polls, elapsed, err := pollUntil(ctx, timeout, interval, func(ctx context.Context) error {
		frame, backend, err := capture(ctx)
		if err != nil {
			return err
		}
		observed = true
		gray := imgutil.GrayDownscale(frame, 0)
		imgW = gray.Bounds().Dx()
		class = fmt.Sprintf("%s/%d", backend, imgW)
		if tmpl == nil {
			if tmpl, err = namedTemplate(store, spec.assertTemplateSpec, backend, imgW); err != nil {
				return err
			}
		}
		region, err := spec.searchRegion(gray)
		if err != nil {
			return err
		}
		x, y, score := imgutil.MatchNCC(region, tmpl)
		if score > best {
			best = score
		}
		if score < spec.threshold {
			return errNotYet
		}
		mx, my = x, y
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, timeoutErr("tap_template: ended early after %s (%d poll(s)): %v", elapsed.Round(time.Millisecond), polls, ctx.Err())
		}
		if isBudgetExhausted(err) {
			if !observed {
				return nil, unavailable("tap_template: no screen capture succeeded within the %s budget (%d poll(s)); cannot match",
					timeout, polls)
			}
			return nil, timeoutErr("tap_template: no match at or above score %.2f within the %s budget (best %.3f, %d poll(s), %s elapsed)",
				spec.threshold, timeout, best, polls, elapsed.Round(time.Millisecond))
		}
		return nil, err
	}
	scale, err := space(ctx, imgW)
	if err != nil {
		return nil, err
	}
	cx := (float64(mx) + float64(tmpl.Bounds().Dx())/2) / scale
	cy := (float64(my) + float64(tmpl.Bounds().Dy())/2) / scale
	if err := tap(ctx, math.Round(cx), math.Round(cy)); err != nil {
		return nil, err
	}
	if spec.name != "" {
		// Best-effort drift bookkeeping: a sidecar write failure never
		// fails the tap that already landed.
		_ = store.RecordScore(spec.bundleID, spec.name, class, best)
	}
	res := map[string]any{
		"matched":    true,
		"x":          math.Round(cx),
		"y":          math.Round(cy),
		"score":      round3(best),
		"threshold":  spec.threshold,
		"elapsed_ms": elapsed.Milliseconds(),
		"polls":      polls,
	}
	if spec.name != "" {
		res["name"] = spec.name
		res["bundle_id"] = spec.bundleID
	}
	return res, nil
}

// tapSpace maps simulator capture pixels onto tap points: the capture
// is native pixels (2x/3x Retina) while taps are points, so the scale
// is the capture width over the viewport width. The width comes from
// the cached last observation, so a dead a11y source (the case the
// template path exists for) does not block the tap; only a target that
// was never observed with a viewport pays for one fresh observe. A
// tree without a readable viewport degrades to 1 (mock-style unscaled
// captures); an unreadable tree with a cold cache is a hard error
// rather than a mis-placed tap. A transiently not-ready bridge (the
// errNotYet observe race) gets a short bounded retry — the pixel match
// already succeeded, so one warm-up blip must not discard it.
func (b *AXeBackend) tapSpace(udid string) tapSpaceFunc {
	return func(ctx context.Context, imgW int) (float64, error) {
		if w := b.cachedViewportWidth(udid); w > 0 {
			if s := float64(imgW) / w; plausibleBackingScale(s) {
				return s, nil
			}
			// The cached width no longer divides the capture into a real
			// backing factor (the screen rotated since the last observe):
			// re-observe rather than mis-place the tap.
		}
		deadline := time.Now().Add(tapSpaceObserveBudget)
		for {
			obs, err := b.observeOnce(ctx, udid, false)
			if err == nil {
				return viewportScale(obs.viewport, imgW), nil
			}
			if !errors.Is(err, errNotYet) || ctx.Err() != nil || time.Now().After(deadline) {
				return 0, unavailable("cannot map capture pixels to tap points (viewport unknown): %v", err)
			}
			select {
			case <-ctx.Done():
				return 0, unavailable("cannot map capture pixels to tap points (viewport unknown): %v", ctx.Err())
			case <-time.After(tapSpaceObserveRetry):
			}
		}
	}
}

// tapSpaceObserveBudget bounds tapSpace's cold-cache retries of a
// transiently not-ready a11y bridge; tapSpaceObserveRetry spaces them.
const (
	tapSpaceObserveBudget = 2 * time.Second
	tapSpaceObserveRetry  = 200 * time.Millisecond
)

// plausibleBackingScale reports whether a capture-to-point scale looks
// like a real screen backing factor (1x–4x, allowing sub-percent
// rounding). A rotation since the cached observe makes the portrait
// point width divide the landscape capture into a non-integer ratio,
// which must not be trusted as a scale.
func plausibleBackingScale(s float64) bool {
	n := math.Round(s)
	return n >= 1 && n <= 4 && math.Abs(s-n) <= 0.02*n
}

// tapSpace maps device capture pixels onto tap coordinates: mirror taps
// are already capture pixels (scale 1); WDA taps are points, mapped via
// the WebDriver window-size metadata call — never the /source
// accessibility snapshot, which is the call the template path exists
// to avoid (it crashes the runner on hostile apps).
func (b *DeviceBackend) tapSpace(udid string) tapSpaceFunc {
	return func(ctx context.Context, imgW int) (float64, error) {
		if b.mirrorFor(udid) != nil {
			return 1, nil
		}
		c, err := b.wdaFor(udid)
		if err != nil {
			return 0, err
		}
		w, _, err := c.WindowSize(ctx)
		if err != nil {
			return 0, unavailable("cannot map capture pixels to tap points (window size unknown): %v", err)
		}
		return float64(imgW) / w, nil
	}
}

// viewportScale derives capture-pixels-per-point from the observed
// viewport width, degrading to 1 when the tree carries no viewport.
func viewportScale(viewport *Frame, imgW int) float64 {
	if viewport == nil || viewport.W <= 0 {
		return 1
	}
	return float64(imgW) / viewport.W
}

// --- backend handlers ---

func handleTapTemplate(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	return tapTemplate(ctx, p, b.templates, b.captureFrame(udid), b.tapSpace(udid),
		func(ctx context.Context, x, y float64) error { return b.tapXY(ctx, udid, x, y) })
}

func handleDeviceTapTemplate(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	if _, err := b.wdaFor(udid); err != nil {
		return nil, err
	}
	return tapTemplate(ctx, p, b.templates, b.captureFrame(udid), b.tapSpace(udid),
		func(ctx context.Context, x, y float64) error { return b.tapXY(ctx, udid, x, y) })
}

func handleMirrorTapTemplate(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.mirrorClient(udid)
	if err != nil {
		return nil, err
	}
	var ack mirror.Ack
	res, err := tapTemplate(ctx, p, b.templates, b.captureFrame(udid), b.tapSpace(udid),
		func(ctx context.Context, x, y float64) error {
			a, err := c.TapAck(ctx, x, y)
			if err != nil {
				return mirrorFail("tap", err)
			}
			ack = a
			return nil
		})
	if err != nil {
		return nil, err
	}
	res["backend"] = "mirror"
	return mirrorAck(udid, "tap", ack, res)
}
