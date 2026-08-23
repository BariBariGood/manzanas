package actions

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
	"github.com/BariBariGood/manzanas/internal/imgutil"
)

// Mirror-backed HID, observe, and composite element actions for physical
// devices (--device-backend <udid>=mirror): macOS iPhone Mirroring driven
// by the mirrord GUI helper (CGEvents + screencapture + Vision OCR)
// instead of WebDriverAgent. Nothing here touches XCTest, so apps that
// kill WDA's accessibility snapshot (TikTok-class) stay drivable.
//
// Fidelity contract: there is no accessibility tree. observe returns
// OCR-derived text boxes and declares it (`backend: "mirror"`,
// `fidelity: "ocr"`); element predicates match on visible text only, and
// composite actions carry the same fidelity fields. Coordinates are
// pixels in the capture image — the space screenshot, observe frames,
// and tap/swipe input all share (NOT device points).

// mirrorFidelity is the observe/composite fidelity declaration for the
// mirror backend: OCR text boxes, not an accessibility tree.
const mirrorFidelity = "ocr"

// Input honesty contract: the helper delivers input either focus-free
// ("background": CGS event records addressed to the mirroring process) or
// by activating the window ("focused": it steals the Mac's focus from
// whoever is using it), and it verifies each gesture by capture delta.
// Every mirror input action journals which mode ran and what the
// verification found, an action the helper could not vouch for fails
// instead of reporting a silent no-op, and focused mode is logged loudly
// because it is a degraded, human-disturbing fallback.

// mirrorBackedKinds routes the action kinds whose mirror implementation
// differs from the WDA one. App lifecycle (install/launch/terminate) is
// devicectl either way and stays in the shared handler map.
var mirrorHandlers = map[string]deviceHandlerFunc{
	"tap":               handleMirrorTap,
	"swipe":             handleMirrorSwipe,
	"scroll":            handleMirrorScroll,
	"type":              handleMirrorType,
	"button":            handleMirrorButton,
	"observe":           handleMirrorObserve,
	"screenshot":        handleMirrorScreenshot,
	"save_template":     handleMirrorSaveTemplate,
	"tap_element":       handleMirrorTapElement,
	"type_into_element": handleMirrorTypeIntoElement,
	"scroll_to_element": handleMirrorScrollToElement,
	"scroll_until":      handleMirrorScrollUntil,
	"scroll_collect":    handleMirrorScrollCollect,
	"wait_for_element":  handleMirrorWaitForElement,
	"wait_tree_stable":  handleMirrorWaitTreeStable,
	"assert_text":       handleMirrorAssertText,
	"assert_no_text":    handleMirrorAssertNoText,
	"assert_template":   handleMirrorAssertTemplate,
	"tap_template":      handleMirrorTapTemplate,
	"wait_for_stable":   handleMirrorWaitPixelStable,
	"pasteboard_get":    handleMirrorPasteboard,
	"pasteboard_set":    handleMirrorPasteboard,
}

// mirrorInputKinds are the mirror action kinds that post input into the
// mirroring window (taps — including long-press —, swipes, wheel
// scrolls, typing, hardware buttons, and the composite element actions
// that end in one of those). They are gated by the per-device
// allow_input opt-in; read-only kinds (observe, screenshot, wait_*)
// are always available. pasteboard_set would be mutating but is
// not_implemented on the mirror either way.
var mirrorInputKinds = map[string]bool{
	"tap":               true,
	"swipe":             true,
	"scroll":            true,
	"type":              true,
	"button":            true,
	"tap_element":       true,
	"type_into_element": true,
	"scroll_to_element": true,
	"scroll_until":      true,
	"scroll_collect":    true,
	"tap_template":      true,
}

// mirrorInputAllowed reports whether mutating mirror actions are allowed
// for a device (the allow_input opt-in; fail-closed by default).
func (b *DeviceBackend) mirrorInputAllowed(udid string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.mirrorInput[udid]
}

// mirrorFor returns the mirror client for a device, or nil when the
// device is WDA-backed. mirror is runtime-mutable (POST /v0/devices,
// SIGHUP config reload), hence the lock.
func (b *DeviceBackend) mirrorFor(udid string) *mirror.Client {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.mirror[udid]
}

// mirrorClient is mirrorFor with an "unavailable" error when the device
// is no longer mirror-backed (a runtime config change can detach the
// mirror between dispatch and the handler running).
func (b *DeviceBackend) mirrorClient(udid string) (*mirror.Client, error) {
	if c := b.mirrorFor(udid); c != nil {
		return c, nil
	}
	return nil, unavailable("device %s is no longer mirror-backed (the devices config changed)", udid)
}

// mirrorFail maps a mirrord failure onto the protocol error shape with
// an actionable message: the blocked/no-window/not-running states need a
// human at the Mac (or the phone), so the error says exactly what to do.
func mirrorFail(op string, err error) error {
	// The error-only client wrappers (used by the element-composite
	// drivers) surface an unverified ack as a typed error; give it the
	// same two-verdict wording as mirrorAckCheck.
	var ue *mirror.UnverifiedError
	if errors.As(err, &ue) {
		if ue.Ack.Verify == "unavailable" {
			return unavailable("mirror %s was posted in %s mode but could not be verified: the helper's check could not run (capture failed mid-action), so whether it reached the phone is unknown — re-observe the screen before retrying, especially for text input",
				op, ue.Ack.Mode)
		}
		return unavailable("mirror %s reported no evidence it reached the phone (mode %s, verify %s): the helper posted the events but the screen did not change, so treat the UI as unchanged and retry",
			op, ue.Ack.Mode, ue.Ack.Verify)
	}
	var me *mirror.Error
	if errors.As(err, &me) {
		switch me.Code {
		case "not-running":
			return unavailable("mirror %s failed: iPhone Mirroring is not running on the host; open the iPhone Mirroring app and connect the phone (this is a physical/GUI step)", op)
		case "no-window":
			return unavailable("mirror %s failed: iPhone Mirroring has no phone window; connect the phone in the app", op)
		case "focus-lost":
			return unavailable("mirror %s failed: %s; another app on the host Mac took focus — bring iPhone Mirroring frontmost (or leave the Mac's GUI session idle) and retry", op, me.Message)
		case "blocked":
			return unavailable("mirror %s failed: iPhone Mirroring is showing a connect/paused interstitial (%s); if it says 'iPhone in Use', lock the physical phone so mirroring can resume — unlocking the phone pauses mirroring", op, me.Message)
		case "input-refused":
			// The helper refused to send anything: a configuration
			// (--background=on) forbade the only path that could deliver.
			// Retrying without changing the helper's config cannot succeed.
			return unavailable("mirror %s was refused, nothing was sent: %s; this is a helper configuration limit, not a transient — change mirrord's --background/MIRROR_BACKGROUND setting on the host rather than retrying", op, me.Message)
		case "input-not-delivered":
			// The helper posted the events and then watched the phone not
			// change. Retryable, but usually means the mirroring session
			// is wedged (or the coordinates hit dead pixels).
			return unavailable("mirror %s was posted but not delivered: %s; retry, and if it persists check the mirroring window is connected and interactive (mirrord --doctor)", op, me.Message)
		case "scroll-not-delivered":
			// Same shape as input-not-delivered, narrowed to the one case
			// the helper cannot retry itself: background scroll records
			// were accepted, nothing changed, and --background=on forbids
			// the focused re-delivery. Retrying is worthwhile — that path
			// lands in bursts — but a host that never lands it needs the
			// focused fallback allowed.
			return unavailable("mirror %s was posted but not delivered: %s; retry (background scroll delivery is intermittent), and allow the focused fallback (--background=auto) on a host where it never lands", op, me.Message)
		case "untypable", "bad-request":
			return badRequest("mirror %s failed: %s", op, me.Message)
		}
		return unavailable("mirror %s failed: %s", op, me.Message)
	}
	return unavailable("mirror %s failed (is the mirrord helper LaunchAgent running in the GUI session?): %v", op, err)
}

// mirrorAck folds an input acknowledgement into the action result:
// journals the delivering mode and the verification evidence, fails the
// action when the helper checked and found no change, and warns when the
// focused fallback ran.
func mirrorAck(udid, op string, ack mirror.Ack, out map[string]any) (map[string]any, error) {
	stampMirrorAck(out, ack)
	if err := mirrorAckCheck(udid, op, ack); err != nil {
		return nil, err
	}
	return out, nil
}

// stampMirrorAck writes an acknowledgement's evidence fields onto a
// result map: the delivering mode, the verification verdict, and any
// per-action note.
func stampMirrorAck(out map[string]any, ack mirror.Ack) {
	if ack.Mode != "" {
		out["input_mode"] = ack.Mode
	}
	if ack.Verify != "" {
		out["verified"] = ack.Verified
		out["verify"] = ack.Verify
	}
	if ack.Delta > 0 {
		out["verify_delta"] = ack.Delta
	}
	if ack.Note != "" {
		out["input_note"] = ack.Note
	}
}

// gestureAckKey carries a per-call gestureAckHolder through the action
// context. The composite element drivers (tapXY/typeInto/swipeXY) return
// only an error, so the ack recorded in the holder is how the composite
// handlers recover the evidence fields for their result maps. Scoping
// the holder to the in-flight call (not the device) keeps concurrent
// actions on one phone from reporting each other's evidence.
type gestureAckKey struct{}

type gestureAckHolder struct {
	mu  sync.Mutex
	ack mirror.Ack
	ok  bool
}

// withGestureAckHolder returns a context carrying a fresh holder for the
// final gesture's ack, plus the holder itself for the caller to read.
func withGestureAckHolder(ctx context.Context) (context.Context, *gestureAckHolder) {
	h := &gestureAckHolder{}
	return context.WithValue(ctx, gestureAckKey{}, h), h
}

// noteMirrorGestureAck records a gesture's ack into the calling
// composite's holder; a dispatch without one (direct kinds, scroll
// loops) is a no-op.
func noteMirrorGestureAck(ctx context.Context, ack mirror.Ack) {
	h, _ := ctx.Value(gestureAckKey{}).(*gestureAckHolder)
	if h == nil {
		return
	}
	h.mu.Lock()
	h.ack, h.ok = ack, true
	h.mu.Unlock()
}

func (h *gestureAckHolder) take() (mirror.Ack, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ack, ok := h.ack, h.ok
	h.ack, h.ok = mirror.Ack{}, false
	return ack, ok
}

// mirrorAckCheck applies mirrorAck's honesty contract where there is no
// result map to stamp (the element-composite drivers, whose interface
// returns only an error): warn on the focused fallback and fail when the
// helper checked and found no change.
func mirrorAckCheck(udid, op string, ack mirror.Ack) error {
	if ack.Mode != "" {
		warnFocusedMirrorInput(udid, op, ack)
	}
	if !ack.Unverified() {
		return nil
	}
	// "unavailable" and a completed-but-negative check are different
	// facts: the first means the events were posted and nobody could
	// look, so a retry can duplicate a tap or re-type text; only the
	// second licenses treating the UI as untouched.
	if ack.Verify == "unavailable" {
		return unavailable("mirror %s was posted in %s mode but could not be verified: the helper's check could not run (capture failed mid-action), so whether it reached the phone is unknown — re-observe the screen before retrying, especially for text input",
			op, ack.Mode)
	}
	return unavailable("mirror %s reported no evidence it reached the phone (mode %s, verify %s): the helper posted the events but the screen did not change, so treat the UI as unchanged and retry",
		op, ack.Mode, ack.Verify)
}

// mirrorFocusWarned remembers the last input mode logged per device so a
// long focused-mode run warns on entry (and on recovery) instead of once
// per tap.
var mirrorFocusWarned struct {
	sync.Mutex
	mode map[string]string
}

// warnFocusedMirrorInput logs the focused fallback loudly: it means every
// action yanks the host Mac's focus away from its user, which is the
// exact failure the background input path exists to avoid.
func warnFocusedMirrorInput(udid, op string, ack mirror.Ack) {
	// Scroll's focused mode is expected, not a degradation: on hosts
	// without the scroll-record symbol it is the only delivery path, and
	// on background-scroll hosts delivery is best-effort — the helper
	// verifies each scroll and falls back to focused on a miss (the ack's
	// note says so). Tracking it would flip the remembered mode on every
	// tap/scroll alternation and flood the log with warning/recovery
	// pairs on a healthy host.
	if op == "scroll" {
		return
	}
	// "focused-degrade" is a deliberate per-action degrade, not a host
	// stuck on the focused fallback: typing needs the window frontmost
	// (iOS routes key events into text input only while iPhone Mirroring
	// is frontmost), so the helper activates it for the duration of the
	// typing and says so in the ack. Logged at Info per action — the
	// window did take the Mac's focus — without flipping the remembered
	// per-device mode.
	if ack.Mode == "focused-degrade" {
		slog.Default().Info("mirror type ran on the focused path: iPhone Mirroring briefly took the host Mac's focus to deliver key events into iOS text input",
			"udid", udid, "action", op, "note", ack.Note)
		return
	}
	mirrorFocusWarned.Lock()
	if mirrorFocusWarned.mode == nil {
		mirrorFocusWarned.mode = map[string]string{}
	}
	prev, seen := mirrorFocusWarned.mode[udid]
	mirrorFocusWarned.mode[udid] = ack.Mode
	mirrorFocusWarned.Unlock()
	if seen && prev == ack.Mode {
		return
	}
	if ack.Mode != "focused" {
		if seen {
			slog.Default().Info("mirror input is focus-free again",
				"udid", udid, "input_mode", ack.Mode)
		}
		return
	}
	slog.Default().Warn("mirror input fell back to FOCUSED mode: every action activates the iPhone Mirroring window and steals the host Mac's focus from its user; keystrokes typed at the Mac can land in the phone",
		"udid", udid, "action", op, "note", ack.Note)
}

func handleMirrorTap(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.mirrorClient(udid)
	if err != nil {
		return nil, err
	}
	x, err := coordField(p, "x")
	if err != nil {
		return nil, err
	}
	y, err := coordField(p, "y")
	if err != nil {
		return nil, err
	}
	if dv, held, err := durationMSField(p); err != nil {
		return nil, err
	} else if held {
		ack, err := c.LongPressAck(ctx, x, y, time.Duration(dv)*time.Millisecond)
		if err != nil {
			return nil, mirrorFail("tap", err)
		}
		// A modern helper echoes the duration it actually ran (it clamps
		// out-of-range values itself); report the helper's word over the
		// requested value when it gives one.
		applied := dv
		if ack.DurationMS > 0 {
			applied = ack.DurationMS
		}
		return mirrorAck(udid, "tap", ack,
			map[string]any{"x": x, "y": y, "duration_ms": applied, "backend": "mirror"})
	}
	ack, err := c.TapAck(ctx, x, y)
	if err != nil {
		return nil, mirrorFail("tap", err)
	}
	return mirrorAck(udid, "tap", ack, map[string]any{"x": x, "y": y, "backend": "mirror"})
}

// handleMirrorScroll posts wheel-scroll events at (x, y): the mirror
// equivalent of a trackpad scroll, useful for fine list movement without
// the momentum a swipe gesture picks up. Positive dy scrolls content
// down; units are capture-image pixels. With the `fallback:"swipe"`
// opt-in, a wheel scroll the helper refused without sending anything
// (scroll needs the focused path, so --background=on refuses it and a
// focus-holding app blocks it) is delivered as the equivalent drag
// instead, with the result saying so.
func handleMirrorScroll(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.mirrorClient(udid)
	if err != nil {
		return nil, err
	}
	x, err := coordField(p, "x")
	if err != nil {
		return nil, err
	}
	y, err := coordField(p, "y")
	if err != nil {
		return nil, err
	}
	dy, err := numField(p, "dy")
	if err != nil {
		return nil, err
	}
	if dy == 0 {
		return nil, badRequest("payload field %q must be a non-zero number", "dy")
	}
	fallback, err := scrollFallbackOpt(p)
	if err != nil {
		return nil, err
	}
	ack, err := c.ScrollAck(ctx, x, y, dy)
	if err != nil {
		if fallback != "swipe" || !mirror.Refused(err) {
			return nil, mirrorFail("scroll", err)
		}
		return mirrorScrollSwipeFallback(ctx, c, udid, x, y, dy, err)
	}
	return mirrorAck(udid, "scroll", ack,
		map[string]any{"x": x, "y": y, "dy": dy, "backend": "mirror"})
}

// scrollFallbackOpt reads scroll's additive `fallback` opt-in: absent
// means no fallback (the default behavior is unchanged); "swipe" is the
// only supported value.
func scrollFallbackOpt(p map[string]any) (string, error) {
	v, ok := p["fallback"]
	if !ok {
		return "", nil
	}
	s, _ := v.(string)
	if s != "swipe" {
		return "", badRequest("payload field %q must be the string %q when present", "fallback", "swipe")
	}
	return s, nil
}

// mirrorScrollSwipeFallback delivers a refused wheel scroll as the
// equivalent drag and stamps the result with what actually happened:
// `method:"swipe-fallback"`, the swipe's geometry, and a note naming the
// refusal. Safe only because the refusal sent nothing to the phone. It
// captures once to learn the bounds the helper will enforce on the
// drag's endpoints (the scroll point alone being in-bounds does not put
// both drag ends in-bounds near an edge).
func mirrorScrollSwipeFallback(ctx context.Context, c *mirror.Client, udid string, x, y, dy float64, refusal error) (map[string]any, error) {
	var me *mirror.Error
	errors.As(refusal, &me)
	var imgW, imgH float64
	if scr, err := c.Screenshot(ctx); err == nil {
		imgW, imgH = float64(scr.ImgW), float64(scr.ImgH)
	}
	// The helper refuses scroll before it bounds-checks the point, so
	// the documented out-of-capture bad_request never ran: enforce it
	// here rather than clamping a bogus point onto a real gesture
	// somewhere the caller never asked for.
	if imgW > 0 && imgH > 0 && (x >= imgW || y >= imgH) {
		return nil, badRequest("mirror scroll failed: point (%v, %v) is outside the mirroring window capture (%dx%d px)",
			x, y, int(imgW), int(imgH))
	}
	x1, y1, x2, y2 := scrollFallbackSwipe(x, y, dy, imgW, imgH)
	ack, err := c.SwipeAck(ctx, x1, y1, x2, y2, mirrorFlickDuration)
	if err != nil {
		// The gesture that failed is the substitute swipe, not the wheel
		// scroll: name it, and the refusal that led here, so the caller
		// knows what was actually posted.
		return nil, mirrorFail("swipe (scroll fallback after "+me.Code+")", err)
	}
	slog.Default().Info("mirror scroll refused; delivered as a swipe (fallback opt-in)",
		"udid", udid, "refusal", me.Code)
	// The gesture that actually ran is a swipe: its ack goes through the
	// swipe honesty contract (the focused-mode warning applies — unlike
	// wheel scroll, a focused swipe IS a degradation — and an unverified
	// ack names the swipe).
	return mirrorAck(udid, "swipe", ack, map[string]any{
		"x": x, "y": y, "dy": dy, "backend": "mirror",
		"method":        "swipe-fallback",
		"swipe_start_x": x1, "swipe_start_y": y1,
		"swipe_end_x": x2, "swipe_end_y": y2,
		"note": fmt.Sprintf("wheel scroll was refused (%s: %s); delivered as a swipe under the fallback opt-in",
			me.Code, me.Message),
	})
}

// scrollFallbackSwipe maps a wheel scroll onto the equivalent drag: a
// swipe centered on (x, y) covering dy capture pixels — positive dy
// (content down) drags the finger up, like swipeForDirection. The drag
// is shifted to keep both endpoints inside the capture (the helper
// rejects out-of-bounds points outright), preserving its length when
// possible; imgW/imgH <= 0 means the bound is unknown and only the 0
// side is enforced.
func scrollFallbackSwipe(x, y, dy, imgW, imgH float64) (x1, y1, x2, y2 float64) {
	y1, y2 = shiftIntoCapture(y+dy/2, y-dy/2, imgH)
	xc, _ := shiftIntoCapture(x, x, imgW)
	return xc, y1, xc, y2
}

// shiftIntoCapture moves a 1-D segment to fit inside the capture's
// valid pixel range [0, bound) — the helper's bounds check is half-open —
// shifting first (length-preserving) and shrinking only when the
// segment is longer than the range. bound <= 0 means the capture size
// is unknown and only the 0 side is enforced.
func shiftIntoCapture(a, b, bound float64) (float64, float64) {
	if bound > 0 {
		if hi := math.Max(a, b); hi > bound-1 {
			a, b = a-(hi-(bound-1)), b-(hi-(bound-1))
		}
	}
	if lo := math.Min(a, b); lo < 0 {
		a, b = a-lo, b-lo
	}
	if bound > 0 {
		a, b = math.Min(a, bound-1), math.Min(b, bound-1)
	}
	return a, b
}

func handleMirrorSwipe(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.mirrorClient(udid)
	if err != nil {
		return nil, err
	}
	var vals [4]float64
	for i, k := range []string{"start_x", "start_y", "end_x", "end_y"} {
		v, err := coordField(p, k)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	duration := 0.5
	if d, ok := p["duration_seconds"]; ok {
		dv, err := toNum(d)
		// The mirrord helper is single-threaded and sleeps for the whole
		// gesture: an unbounded duration wedges every later action.
		if err != nil || dv <= 0 || dv > 10 {
			return nil, badRequest("duration_seconds must be a positive number of at most 10")
		}
		duration = dv
	}
	ack, err := c.SwipeAck(ctx, vals[0], vals[1], vals[2], vals[3],
		time.Duration(duration*float64(time.Second)))
	if err != nil {
		return nil, mirrorFail("swipe", err)
	}
	return mirrorAck(udid, "swipe", ack, map[string]any{
		"start_x": vals[0], "start_y": vals[1],
		"end_x": vals[2], "end_y": vals[3], "backend": "mirror"})
}

func handleMirrorType(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.mirrorClient(udid)
	if err != nil {
		return nil, err
	}
	text, ok := p["text"].(string)
	if !ok || text == "" {
		return nil, badRequest("type requires a non-empty string payload field %q", "text")
	}
	opts, err := typeOptsFromPayload(p)
	if err != nil {
		return nil, err
	}
	if err := validateMirrorTypeOpts(opts); err != nil {
		return nil, err
	}
	ack, err := c.TypeAck(ctx, text)
	if err != nil {
		return nil, mirrorFail("type", err)
	}
	return mirrorAck(udid, "type", ack,
		map[string]any{"typed_runes": len([]rune(text)), "backend": "mirror"})
}

// validateMirrorTypeOpts rejects typing options the mirror backend cannot
// service, before any UI mutation.
func validateMirrorTypeOpts(opts typeOpts) error {
	if opts.strategy == typeStrategyPaste {
		return notImplemented("the %q typing strategy is simulator-only; the mirror backend types via raw HID keycodes through iPhone Mirroring", typeStrategyPaste)
	}
	if opts.requireFocus {
		return notImplemented("require_focus is unavailable on the mirror backend: OCR observation cannot see the on-screen keyboard's accessibility elements")
	}
	return nil
}

// mirrorButtons maps the protocol's button names onto iPhone Mirroring
// keyboard shortcuts. Lock/volume are physical-device functions the
// mirror cannot reach.
var mirrorButtons = map[string]string{
	"home": "cmd+1",
}

func handleMirrorButton(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.mirrorClient(udid)
	if err != nil {
		return nil, err
	}
	name, _ := p["name"].(string)
	combo, ok := mirrorButtons[name]
	if !ok {
		if name == "lock" || name == "volume-up" || name == "volume-down" {
			return nil, notImplemented("button %q is not reachable through iPhone Mirroring (only %q is; lock/volume are physical-phone functions)", name, "home")
		}
		return nil, badRequest("button on a mirror-backed device requires payload field %q, one of: home", "name")
	}
	ack, err := c.PressAck(ctx, combo)
	if err != nil {
		return nil, mirrorFail("button", err)
	}
	return mirrorAck(udid, "button", ack,
		map[string]any{"button": name, "backend": "mirror"})
}

func handleMirrorScreenshot(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.mirrorClient(udid)
	if err != nil {
		return nil, err
	}
	if _, err := boolFlag(p, "inline", true); err != nil {
		return nil, err
	}
	format, quality, maxDim, err := screenshotParams(p)
	if err != nil {
		return nil, err
	}
	scr, err := c.Screenshot(ctx)
	if err != nil {
		return nil, mirrorFail("screenshot", err)
	}
	img := scr.PNG
	if format != "png" || maxDim > 0 {
		img, err = imgutil.Transcode(img, format, quality, maxDim)
		if err != nil {
			return nil, internal("screenshot transform failed: %v", err)
		}
	}
	sum := sha256.Sum256(img)
	// img_w/img_h are the pre-transform capture size — the tap/observe
	// coordinate space — so callers of size-capped screenshots can map
	// what they see back to tappable coordinates.
	out := map[string]any{
		"format":           format,
		"bytes":            len(img),
		"sha256":           hex.EncodeToString(sum[:]),
		"backend":          "mirror",
		"img_w":            scr.ImgW,
		"img_h":            scr.ImgH,
		format + "_base64": base64.StdEncoding.EncodeToString(img),
	}
	if scr.Scale > 0 {
		out["capture_scale"] = scr.Scale
	}
	return out, nil
}

// mirrorOCRAutoMaxDim is the default long-edge pixel cap sent with every
// recognition pass: oversized captures (an enlarged mirroring window on a
// high-resolution display) are downscaled before Vision runs instead of
// paying full-frame recognition latency. The helper never downscales
// frames at or below 640px wide, so typical ~636px-wide 2x mirror
// captures — and 1x low-resolution ones — pass through untouched.
const mirrorOCRAutoMaxDim = 1300

// mirrorOCROptionsFromPayload builds the recognition options for one
// observe pass: `ocr_level` ("fast" | "accurate"), `rect` ({x,y,w,h} in
// capture-image pixels), and the resolution-aware auto-downscale default.
func mirrorOCROptionsFromPayload(p map[string]any) (mirror.OCROptions, error) {
	opts := mirror.OCROptions{MaxDim: mirrorOCRAutoMaxDim}
	if v, ok := p["ocr_level"]; ok {
		s, ok := v.(string)
		if !ok || (s != "fast" && s != "accurate") {
			return opts, badRequest("payload field %q must be %q or %q", "ocr_level", "fast", "accurate")
		}
		opts.Level = s
	}
	if raw, ok := p["rect"]; ok {
		m, ok := raw.(map[string]any)
		if !ok {
			return opts, badRequest("payload field %q must be an object with x, y, w, h (capture-image pixels)", "rect")
		}
		r := mirror.OCRRect{}
		for _, kv := range []struct {
			key string
			dst *float64
		}{{"x", &r.X}, {"y", &r.Y}, {"w", &r.W}, {"h", &r.H}} {
			v, err := numField(m, kv.key)
			if err != nil {
				return opts, badRequest("rect field %q must be a number", kv.key)
			}
			*kv.dst = v
		}
		if r.X < 0 || r.Y < 0 {
			return opts, badRequest("rect requires non-negative x and y")
		}
		if r.W <= 0 || r.H <= 0 {
			return opts, badRequest("rect requires positive w and h")
		}
		opts.Rect = &r
	}
	return opts, nil
}

// mirrorOCR runs one recognition pass. When only the auto-downscale
// default is in play (no caller-requested level/rect) and the helper
// predates the POST /v1/ocr options route ("no route"), it falls back to
// the legacy full-resolution GET so older helpers keep working. Options
// the caller explicitly asked for stay a hard error — silently ignoring
// them would misrepresent what recognition ran.
func mirrorOCR(ctx context.Context, c *mirror.Client, opts mirror.OCROptions) (mirror.OCRResult, error) {
	res, err := c.OCR(ctx, opts)
	if err != nil && opts.Level == "" && opts.Rect == nil && mirrorOCRNoRoute(err) {
		return c.OCR(ctx, mirror.OCROptions{})
	}
	return res, err
}

// mirrorOCRNoRoute reports the pre-options helper's 404 for POST /v1/ocr.
func mirrorOCRNoRoute(err error) bool {
	var me *mirror.Error
	return errors.As(err, &me) && me.Code == "bad-request" && strings.HasPrefix(me.Message, "no route ")
}

func handleMirrorObserve(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	includeRaw, err := boolFlag(p, "include_raw", false)
	if err != nil {
		return nil, err
	}
	if _, err := boolFlag(p, "refresh", false); err != nil {
		return nil, err
	}
	format, err := observeFormat(p)
	if err != nil {
		return nil, err
	}
	filter, err := treeFilterFromPayload(p)
	if err != nil {
		return nil, err
	}
	opts, err := mirrorOCROptionsFromPayload(p)
	if err != nil {
		return nil, err
	}
	c, err := b.mirrorClient(udid)
	if err != nil {
		return nil, err
	}
	res, err := mirrorOCR(ctx, c, opts)
	if err != nil {
		return nil, mirrorFail("observe", err)
	}
	nodes := ocrNodes(res.Boxes)
	viewport := &Frame{W: float64(res.ImgW), H: float64(res.ImgH)}
	shown, err := scopeSubtree(p, nodes, viewport)
	if err != nil {
		return nil, err
	}
	scoped := len(shown) != len(nodes) || (len(shown) > 0 && shown[0] != nodes[0])
	if filter.active() {
		shown = filterNodes(shown, filter)
	}
	if shown == nil {
		shown = []*Node{}
	}
	out := map[string]any{
		"hash":     TreeHash(nodes),
		"backend":  "mirror",
		"fidelity": mirrorFidelity,
		"img_w":    res.ImgW,
		"img_h":    res.ImgH,
		"note":     "OCR-derived text boxes from the iPhone Mirroring window, not an accessibility tree; coordinates are capture-image pixels",
	}
	if filter.interactiveOnly {
		out["note"] = out["note"].(string) + "; interactive_only is a no-op here: OCR cannot determine interactability, every node carries interactable:true as a compatibility constant"
	}
	if res.Scale > 0 {
		out["capture_scale"] = res.Scale
	}
	// Honesty metadata: what recognition actually ran, as the helper
	// applied it — not what the caller asked for.
	out["served_scale"] = res.ServedScale
	out["ocr_level"] = res.Level
	if res.Rect != nil {
		out["rect"] = res.Rect
	}
	if format == "compact" {
		out["format"] = "compact"
		out["tree_compact"] = compactTreeText(shown)
	} else {
		out["tree"] = shown
	}
	if filter.active() || scoped {
		out["total_elements"] = countNodes(nodes)
		out["returned_elements"] = countNodes(shown)
	}
	if includeRaw {
		out["raw"] = res.Boxes
		out["raw_format"] = "vision-ocr-boxes"
	}
	return out, nil
}

// ocrNodes converts OCR boxes into the shared compacted-node shape so the
// matcher, filters, and composite actions run unchanged. Every node is a
// flat "Text" leaf: predicates on role/id/placeholder/value cannot match
// (there is no tree to source them from) — text labels only. Interactable
// is a compatibility CONSTANT, not a measurement: OCR cannot tell a
// button from a caption, but the v0 wire shape has always carried
// interactable on mirror nodes and dropping a response field is a
// non-additive change. It is uniform, so the predicate ranker's
// interactable term never changes which node wins and
// `interactive_only` filters keep everything; observes with that flag
// say so in the result note.
func ocrNodes(boxes []mirror.OCRBox) []*Node {
	nodes := make([]*Node, 0, len(boxes))
	for _, o := range boxes {
		nodes = append(nodes, &Node{
			Role:         "Text",
			Label:        o.Text,
			Frame:        &Frame{X: o.X, Y: o.Y, W: o.W, H: o.H},
			Interactable: true,
		})
	}
	return nodes
}

// withMirrorFidelity stamps a composite action result with the mirror
// backend's fidelity declaration, so callers know element resolution was
// OCR text matching rather than accessibility-tree predicates.
func withMirrorFidelity(res map[string]any, err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	res["backend"] = "mirror"
	res["fidelity"] = mirrorFidelity
	return res, nil
}

// withMirrorGesture is withMirrorFidelity plus the evidence fields from
// the composite's final gesture (the tap or keystrokes recorded by the
// element drivers), so composite results carry the same
// input_mode/verified stamps as the direct input kinds.
func withMirrorGesture(acks *gestureAckHolder, res map[string]any, err error) (map[string]any, error) {
	res, err = withMirrorFidelity(res, err)
	if err != nil {
		return nil, err
	}
	if ack, ok := acks.take(); ok {
		stampMirrorAck(res, ack)
	}
	return res, nil
}

func handleMirrorTapElement(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	ctx, acks := withGestureAckHolder(ctx)
	res, err := elemTap(ctx, b, udid, p)
	return withMirrorGesture(acks, res, err)
}

func handleMirrorTypeIntoElement(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	// Reject unsupported typing options before the focusing tap runs:
	// elemTypeInto's own require_focus guard polls for keyboard elements
	// OCR can never see, so it must not get that far.
	opts, err := typeOptsFromPayload(p)
	if err != nil {
		return nil, err
	}
	if err := validateMirrorTypeOpts(opts); err != nil {
		return nil, err
	}
	ctx, acks := withGestureAckHolder(ctx)
	res, err := elemTypeInto(ctx, b, udid, p)
	return withMirrorGesture(acks, res, err)
}

func handleMirrorScrollToElement(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	ctx, acks := withGestureAckHolder(ctx)
	res, err := elemScrollTo(ctx, b, udid, p)
	return withMirrorGesture(acks, res, err)
}

func handleMirrorWaitForElement(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	return withMirrorFidelity(elemWaitFor(ctx, b, udid, p))
}

func handleMirrorWaitTreeStable(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	return withMirrorFidelity(elemTreeStable(ctx, b, udid, p))
}

func handleMirrorPasteboard(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	return nil, notImplemented("pasteboard access is unavailable on the mirror backend: iPhone Mirroring exposes no pasteboard channel (typing goes through raw HID keycodes)")
}

// mirrorObserveTransient reports mirrord failures a wait loop should ride
// out within its own budget: momentary capture hiccups while the window
// re-composites or gets re-activated. blocked/no-window stay hard
// failures — clearing them ("iPhone in Use", disconnected phone) takes a
// human at the phone, so waiting out the budget would only replace the
// actionable 503 with a misleading element-not-found timeout.
func mirrorObserveTransient(err error) bool {
	var me *mirror.Error
	return errors.As(err, &me) && me.Code == "capture-failed"
}

// mirrorObserveOnce implements the elementDriver observe poll for
// mirror-backed devices: one OCR pass over the window capture. Transient
// failures come back as errNotYet so wait loops keep polling, mirroring
// the WDA transport-blip behavior in observeOnce.
func (b *DeviceBackend) mirrorObserveOnce(ctx context.Context, udid string) (observation, error) {
	c, err := b.mirrorClient(udid)
	if err != nil {
		return observation{}, err
	}
	res, err := mirrorOCR(ctx, c, mirror.OCROptions{MaxDim: mirrorOCRAutoMaxDim})
	if err != nil {
		if mirrorObserveTransient(err) {
			return observation{}, errNotYet
		}
		return observation{}, mirrorFail("observe", err)
	}
	return observation{
		nodes:    ocrNodes(res.Boxes),
		viewport: &Frame{W: float64(res.ImgW), H: float64(res.ImgH)},
	}, nil
}
