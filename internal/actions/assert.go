package actions

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register decoders for template/frame payloads
	_ "image/png"
	"math"
	"strings"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
	"github.com/BariBariGood/manzanas/internal/imgutil"
	"github.com/BariBariGood/manzanas/internal/templates"
)

// Verification action kinds: assert_text / assert_no_text check the
// backend's observe source (a11y tree text on simulators and WDA
// devices, OCR text boxes on mirror-backed devices) for visible text,
// and assert_template matches an inline image template against the
// current screen via normalized cross-correlation. All three retry
// within a short bounded budget (timeout_ms, default 3 s) so a screen
// still settling after an action gets one more look before the
// assertion fails. A failed assertion is an action error with the
// dedicated code "assertion_failed" — distinct from timeout (a wait
// that never matched) because the caller asserted a postcondition and
// the daemon observed it to be false.

const (
	// defaultAssertTimeout bounds the retry-after-settle window: long
	// enough for one settle + re-observe, short enough that a genuinely
	// failed assertion reports quickly.
	defaultAssertTimeout = 3 * time.Second

	// defaultTemplateThreshold is the NCC score at or above which an
	// assert_template match counts as found.
	defaultTemplateThreshold = 0.8

	// maxTemplateBytes caps the decoded template_base64 payload: a
	// template is a small crop of a screen, never megabytes.
	maxTemplateBytes = 4 << 20

	// maxTemplateDim caps the decoded template's pixel dimensions
	// (checked via DecodeConfig before decoding), so a tiny highly
	// compressed blob cannot expand into a huge raster and the NCC
	// search cost stays bounded.
	maxTemplateDim = 4096
)

func assertionFailed(format string, a ...any) *Error {
	return &Error{Code: "assertion_failed", Message: fmt.Sprintf(format, a...)}
}

// textRun is one visible text run with its OCR confidence (1.0 on tree
// backends, where text is exact rather than recognized).
type textRun struct {
	text string
	conf float64
}

// textObserveFunc returns one poll of the backend's visible text runs.
// errNotYet keeps the assertion's retry loop polling.
type textObserveFunc func(ctx context.Context) ([]textRun, error)

// flattenTextRuns reduces a compacted node tree to its text runs
// (label, value, placeholder), each with confidence 1.
func flattenTextRuns(nodes []*Node) []textRun {
	var out []textRun
	var walk func(ns []*Node)
	walk = func(ns []*Node) {
		for _, n := range ns {
			if n == nil {
				continue
			}
			for _, s := range []string{n.Label, n.Value, n.Placeholder} {
				if s != "" {
					out = append(out, textRun{text: s, conf: 1})
				}
			}
			walk(n.Children)
		}
	}
	walk(nodes)
	return out
}

// assertTextSpec is the parsed assert_text / assert_no_text payload.
type assertTextSpec struct {
	text    string
	fuzzy   bool
	minConf float64
}

func assertTextSpecFromPayload(p map[string]any) (assertTextSpec, error) {
	s := assertTextSpec{fuzzy: true}
	text, ok := p["text"].(string)
	if !ok || strings.TrimSpace(text) == "" {
		return s, badRequest("assert requires a non-empty string payload field %q", "text")
	}
	s.text = text
	fuzzy, err := boolFlag(p, "fuzzy", true)
	if err != nil {
		return s, err
	}
	s.fuzzy = fuzzy
	if v, ok := p["min_confidence"]; ok {
		n, err := toNum(v)
		if err != nil || n < 0 || n > 1 {
			return s, badRequest("payload field %q must be a number in [0, 1]", "min_confidence")
		}
		s.minConf = n
	}
	return s, nil
}

// matchText finds the first run matching the spec: case-insensitive
// substring by default, case-insensitive whole-run equality with
// fuzzy:false. Runs below min_confidence are skipped.
func (s assertTextSpec) matchText(runs []textRun) (textRun, bool) {
	want := strings.ToLower(strings.TrimSpace(s.text))
	for _, r := range runs {
		if r.conf < s.minConf {
			continue
		}
		got := strings.ToLower(strings.TrimSpace(r.text))
		if (s.fuzzy && strings.Contains(got, want)) || (!s.fuzzy && got == want) {
			return r, true
		}
	}
	return textRun{}, false
}

// assertText polls the observe source until the text appears, failing
// with assertion_failed when the budget expires without a match.
func assertText(ctx context.Context, p map[string]any, obs textObserveFunc) (map[string]any, error) {
	spec, err := assertTextSpecFromPayload(p)
	if err != nil {
		return nil, err
	}
	timeout, interval, err := waitParams(p, defaultAssertTimeout)
	if err != nil {
		return nil, err
	}
	var matched textRun
	observed := false
	polls, elapsed, err := pollUntil(ctx, timeout, interval, func(ctx context.Context) error {
		runs, err := obs(ctx)
		if err != nil {
			return err
		}
		observed = true
		hit, ok := spec.matchText(runs)
		if !ok {
			return errNotYet
		}
		matched = hit
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, timeoutErr("assert_text: ended early after %s (%d poll(s)): %v", elapsed.Round(time.Millisecond), polls, ctx.Err())
		}
		if isBudgetExhausted(err) {
			// Presence cannot be asserted false from zero observations
			// (mirroring assertNoText's guard): a budget where every poll
			// failed to read the screen is an infrastructure hiccup, not
			// an observed postcondition.
			if !observed {
				return nil, unavailable("assert_text: the screen produced no readable observation within the %s budget (%d poll(s)); cannot assert presence",
					timeout, polls)
			}
			return nil, assertionFailed("assert_text: %q not visible within the %s budget (%d poll(s), %s elapsed)",
				spec.text, timeout, polls, elapsed.Round(time.Millisecond))
		}
		return nil, err
	}
	return map[string]any{
		"asserted":   "text",
		"found":      true,
		"text":       spec.text,
		"matched":    matched.text,
		"elapsed_ms": elapsed.Milliseconds(),
		"polls":      polls,
	}, nil
}

// assertNoText polls the observe source until no run matches the text
// (so a toast still animating out gets a settle window), failing with
// assertion_failed when a match persists through the whole budget.
func assertNoText(ctx context.Context, p map[string]any, obs textObserveFunc) (map[string]any, error) {
	spec, err := assertTextSpecFromPayload(p)
	if err != nil {
		return nil, err
	}
	timeout, interval, err := waitParams(p, defaultAssertTimeout)
	if err != nil {
		return nil, err
	}
	var lastHit textRun
	observed := false
	polls, elapsed, err := pollUntil(ctx, timeout, interval, func(ctx context.Context) error {
		runs, err := obs(ctx)
		if err != nil {
			return err
		}
		observed = true
		if hit, ok := spec.matchText(runs); ok {
			lastHit = hit
			return errNotYet
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, timeoutErr("assert_no_text: ended early after %s (%d poll(s)): %v", elapsed.Round(time.Millisecond), polls, ctx.Err())
		}
		if isBudgetExhausted(err) {
			// Distinguish "the text kept matching" from "the screen was
			// never readable" (a11y bridge warming, transient mirror
			// capture failures): absence cannot be asserted from zero
			// observations.
			if !observed {
				return nil, unavailable("assert_no_text: the screen produced no readable observation within the %s budget (%d poll(s)); cannot assert absence",
					timeout, polls)
			}
			return nil, assertionFailed("assert_no_text: %q still visible (matched %q) after the %s budget (%d poll(s), %s elapsed)",
				spec.text, lastHit.text, timeout, polls, elapsed.Round(time.Millisecond))
		}
		return nil, err
	}
	return map[string]any{
		"asserted":   "no_text",
		"absent":     true,
		"text":       spec.text,
		"elapsed_ms": elapsed.Milliseconds(),
		"polls":      polls,
	}, nil
}

// isBudgetExhausted reports whether a pollUntil failure means the wait
// budget ran out (as opposed to a hard backend error). Callers must
// first rule out the caller's own context ending (ctx.Err() != nil):
// a request cut short never observed the postcondition to be false, so
// it is a timeout, not an assertion failure (see wait_stable.go).
func isBudgetExhausted(err error) bool {
	return errors.Is(err, errNotYet) || errors.Is(err, context.DeadlineExceeded)
}

// round3 keeps float result fields readable on the wire.
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// frameCaptureFunc returns one decoded capture of the target's screen
// plus the backend name that produced it (which keys the template
// store's device class).
type frameCaptureFunc func(ctx context.Context) (image.Image, string, error)

// assertTemplateSpec is the parsed assert_template payload: an inline
// decoded template, or a named store lookup (tmpl nil until the first
// capture reveals the device class).
type assertTemplateSpec struct {
	tmpl      *image.Gray
	name      string
	bundleID  string
	threshold float64
}

func assertTemplateSpecFromPayload(p map[string]any, store *templates.Store) (assertTemplateSpec, error) {
	var s assertTemplateSpec
	_, hasName := p["name"]
	_, hasInline := p["template_base64"]
	if hasName && hasInline {
		return s, badRequest("pass %q or %q, not both", "template_base64", "name")
	}
	switch {
	case hasName:
		name, _ := p["name"].(string)
		if name == "" {
			return s, badRequest("payload field %q must be a non-empty string", "name")
		}
		if err := templates.ValidateName(name); err != nil {
			return s, badRequest("%v", err)
		}
		// Reject before any capture is taken, like save_template: a
		// capture is real work (mirror captures drive the exclusive
		// GUI helper).
		if store == nil {
			return s, unavailable("the template library is not configured on this daemon (--templates-dir)")
		}
		s.name = name
		s.bundleID = defaultTemplateBundle
		if v, ok := p["bundle_id"]; ok {
			b, _ := v.(string)
			if b == "" {
				return s, badRequest("payload field %q must be a non-empty string when present", "bundle_id")
			}
			if err := templates.ValidateBundleID(b); err != nil {
				return s, badRequest("%v", err)
			}
			s.bundleID = b
		}
	case hasInline:
		b64, _ := p["template_base64"].(string)
		if b64 == "" {
			return s, badRequest("payload field %q must be a non-empty string", "template_base64")
		}
		if len(b64) > maxTemplateBytes*4/3+4 {
			return s, badRequest("payload field %q exceeds the %d-byte template limit", "template_base64", maxTemplateBytes)
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return s, badRequest("payload field %q is not valid base64: %v", "template_base64", err)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			return s, badRequest("payload field %q does not decode as an image: %v", "template_base64", err)
		}
		if cfg.Width > maxTemplateDim || cfg.Height > maxTemplateDim {
			return s, badRequest("template is %dx%d, larger than the %dpx-per-side limit", cfg.Width, cfg.Height, maxTemplateDim)
		}
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			return s, badRequest("payload field %q does not decode as an image: %v", "template_base64", err)
		}
		s.tmpl = imgutil.GrayDownscale(img, 0)
		if s.tmpl.Bounds().Dx() < 2 || s.tmpl.Bounds().Dy() < 2 {
			return s, badRequest("template is too small to match (min 2x2 pixels)")
		}
	default:
		return s, badRequest("template actions require payload field %q (base64 PNG/JPEG) or %q", "template_base64", "name")
	}
	s.threshold = defaultTemplateThreshold
	if v, ok := p["threshold"]; ok {
		n, err := toNum(v)
		if err != nil || n <= 0 || n > 1 {
			return s, badRequest("payload field %q must be a number in (0, 1]", "threshold")
		}
		s.threshold = n
	}
	return s, nil
}

// namedTemplate resolves a stored template for the capture's device
// class ("<backend>/<img_w>", the key save_template stored it under).
func namedTemplate(store *templates.Store, spec assertTemplateSpec, backend string, imgW int) (*image.Gray, error) {
	class := fmt.Sprintf("%s/%d", backend, imgW)
	raw, _, err := store.Get(spec.bundleID, spec.name, class)
	if err != nil {
		return nil, &Error{Code: "not_found", Message: fmt.Sprintf(
			"%v (templates are stored per device class; save one from this backend with save_template)", err)}
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, internal("stored template did not decode: %v", err)
	}
	return imgutil.GrayDownscale(img, 0), nil
}

// assertTemplate polls screen captures until the template matches at or
// above the threshold, failing with assertion_failed (carrying the best
// score seen) when the budget expires.
func assertTemplate(ctx context.Context, p map[string]any, store *templates.Store, capture frameCaptureFunc) (map[string]any, error) {
	spec, err := assertTemplateSpecFromPayload(p, store)
	if err != nil {
		return nil, err
	}
	timeout, interval, err := waitParams(p, defaultAssertTimeout)
	if err != nil {
		return nil, err
	}
	tmpl := spec.tmpl
	var mx, my int
	var best float64
	observed := false
	polls, elapsed, err := pollUntil(ctx, timeout, interval, func(ctx context.Context) error {
		frame, backend, err := capture(ctx)
		if err != nil {
			return err
		}
		observed = true
		gray := imgutil.GrayDownscale(frame, 0)
		if tmpl == nil {
			if tmpl, err = namedTemplate(store, spec, backend, gray.Bounds().Dx()); err != nil {
				return err
			}
		}
		x, y, score := imgutil.MatchNCC(gray, tmpl)
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
			return nil, timeoutErr("assert_template: ended early after %s (%d poll(s)): %v", elapsed.Round(time.Millisecond), polls, ctx.Err())
		}
		if isBudgetExhausted(err) {
			if !observed {
				return nil, unavailable("assert_template: no screen capture succeeded within the %s budget (%d poll(s)); cannot assert a match",
					timeout, polls)
			}
			return nil, assertionFailed("assert_template: no match at or above score %.2f within the %s budget (best %.3f, %d poll(s), %s elapsed)",
				spec.threshold, timeout, best, polls, elapsed.Round(time.Millisecond))
		}
		return nil, err
	}
	res := map[string]any{
		"asserted":   "template",
		"found":      true,
		"x":          mx,
		"y":          my,
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

// --- simulator (AXe/mock) handlers ---

func (b *AXeBackend) textRunsOnce(ctx context.Context, udid string) ([]textRun, error) {
	obs, err := b.observeOnce(ctx, udid, false)
	if err != nil {
		return nil, err
	}
	return flattenTextRuns(obs.nodes), nil
}

func handleAssertText(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	return assertText(ctx, p, func(ctx context.Context) ([]textRun, error) {
		return b.textRunsOnce(ctx, udid)
	})
}

func handleAssertNoText(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	return assertNoText(ctx, p, func(ctx context.Context) ([]textRun, error) {
		return b.textRunsOnce(ctx, udid)
	})
}

func handleAssertTemplate(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	return assertTemplate(ctx, p, b.templates, b.captureFrame(udid))
}

// captureFrame returns the simulator screenshot path as a decoded frame
// capture (used by assert_template and verified taps).
func (b *AXeBackend) captureFrame(udid string) frameCaptureFunc {
	return func(ctx context.Context) (image.Image, string, error) {
		raw, backend, err := b.capturePNG(ctx, udid)
		if err != nil {
			return nil, "", err
		}
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, "", internal("screenshot did not decode: %v", err)
		}
		return img, backend, nil
	}
}

// --- WDA device handlers ---

func (b *DeviceBackend) textRunsOnce(ctx context.Context, udid string) ([]textRun, error) {
	obs, err := b.observeOnce(ctx, udid, false)
	if err != nil {
		return nil, err
	}
	return flattenTextRuns(obs.nodes), nil
}

func handleDeviceAssertText(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	if _, err := b.wdaFor(udid); err != nil {
		return nil, err
	}
	return assertText(ctx, p, func(ctx context.Context) ([]textRun, error) {
		return b.textRunsOnce(ctx, udid)
	})
}

func handleDeviceAssertNoText(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	if _, err := b.wdaFor(udid); err != nil {
		return nil, err
	}
	return assertNoText(ctx, p, func(ctx context.Context) ([]textRun, error) {
		return b.textRunsOnce(ctx, udid)
	})
}

func handleDeviceAssertTemplate(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	if b.mirrorFor(udid) == nil {
		if _, err := b.wdaFor(udid); err != nil {
			return nil, err
		}
	}
	return assertTemplate(ctx, p, b.templates, b.captureFrame(udid))
}

// captureFrame returns the device's screenshot path as a decoded frame
// capture: the mirror window capture when mirror-backed, WDA otherwise.
func (b *DeviceBackend) captureFrame(udid string) frameCaptureFunc {
	return func(ctx context.Context) (image.Image, string, error) {
		var raw []byte
		backend := "wda"
		if m := b.mirrorFor(udid); m != nil {
			backend = "mirror"
			scr, err := m.Screenshot(ctx)
			if err != nil {
				return nil, "", mirrorFail("screenshot", err)
			}
			raw = scr.PNG
		} else {
			c, err := b.wdaFor(udid)
			if err != nil {
				return nil, "", err
			}
			raw, err = c.Screenshot(ctx)
			if err != nil {
				return nil, "", b.wdaFail(udid, "screenshot", err)
			}
		}
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, "", internal("screenshot did not decode: %v", err)
		}
		return img, backend, nil
	}
}

// --- mirror handlers ---

// mirrorTextRunsOnce polls the mirror OCR source, preserving per-box
// confidence so min_confidence filters on real recognition confidence
// rather than the tree backends' constant 1.0.
func (b *DeviceBackend) mirrorTextRunsOnce(ctx context.Context, udid string) ([]textRun, error) {
	c, err := b.mirrorClient(udid)
	if err != nil {
		return nil, err
	}
	res, err := mirrorOCR(ctx, c, mirror.OCROptions{MaxDim: mirrorOCRAutoMaxDim})
	if err != nil {
		if mirrorObserveTransient(err) {
			return nil, errNotYet
		}
		return nil, mirrorFail("observe", err)
	}
	runs := make([]textRun, 0, len(res.Boxes))
	for _, box := range res.Boxes {
		runs = append(runs, textRun{text: box.Text, conf: box.Confidence})
	}
	return runs, nil
}

func handleMirrorAssertText(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	return withMirrorFidelity(assertText(ctx, p, func(ctx context.Context) ([]textRun, error) {
		return b.mirrorTextRunsOnce(ctx, udid)
	}))
}

func handleMirrorAssertNoText(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	return withMirrorFidelity(assertNoText(ctx, p, func(ctx context.Context) ([]textRun, error) {
		return b.mirrorTextRunsOnce(ctx, udid)
	}))
}

func handleMirrorAssertTemplate(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	res, err := assertTemplate(ctx, p, b.templates, b.captureFrame(udid))
	if err != nil {
		return nil, err
	}
	res["backend"] = "mirror"
	return res, nil
}
