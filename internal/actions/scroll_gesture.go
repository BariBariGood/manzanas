package actions

import (
	"context"
	"errors"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
)

// List-gesture abstraction for the scrolling composites (scroll_until /
// scroll_collect): one "advance the list by roughly one screen" gesture,
// chosen per backend. Simulators and WDA devices swipe; mirror-backed
// devices prefer a wheel scroll (iPhone Mirroring translates trackpad
// wheel events without the momentum a drag picks up) and fall back to a
// fast flick when the wheel path is unavailable.

// listGestureDriver is the gesture primitive the scrolling composites
// are built on. region is the rectangle the gesture happens in (usually
// the viewport); direction is the content edge to reveal (down = content
// below the fold).
type listGestureDriver interface {
	scrollGesture(ctx context.Context, udid string, region Frame, direction string) error
}

// errGestureNoEffect reports a gesture that was delivered to the screen
// but verifiably changed nothing (mirrord's per-action pixel check ran
// and saw no delta). For taps and typing that is a failure; for a list
// scroll it is the expected shape of hitting the end of the list — the
// end-of-list detector NEEDS a no-change gesture to succeed — so the
// scrolling loops fold it into the non-move counter instead of failing.
var errGestureNoEffect = errors.New("gesture delivered but the screen did not change")

// gestureNoEffect reports whether a mirror input error means "posted,
// verified, nothing happened": the typed input-not-delivered /
// scroll-not-delivered failures, or an unverified ack whose check
// actually ran. verify == "unavailable" stays fatal — nobody looked, so
// the screen may well have moved.
func gestureNoEffect(err error) bool {
	var ue *mirror.UnverifiedError
	if errors.As(err, &ue) {
		return ue.Ack.Verify != "unavailable"
	}
	var me *mirror.Error
	if !errors.As(err, &me) {
		return false
	}
	return me.Code == "input-not-delivered" || me.Code == "scroll-not-delivered"
}

// mirrorFlickDuration makes the fallback swipe a momentum flick: a fast
// short drag advances an iOS list about a screen, where a slow drag
// barely moves it.
const mirrorFlickDuration = 150 * time.Millisecond

// scrollGesture implements listGestureDriver for simulators: one swipe
// across the region at the standard scroll fraction.
func (b *AXeBackend) scrollGesture(ctx context.Context, udid string, region Frame, direction string) error {
	x1, y1, x2, y2 := swipeForDirection(region, direction)
	return b.swipeXY(ctx, udid, x1, y1, x2, y2)
}

// scrollGesture implements listGestureDriver for devices. Mirror-backed
// devices scroll vertically with wheel events first — falling back to a
// flick when the wheel path fails for a reason a flick can survive (an
// older mirrord without /v1/scroll) — and flick horizontally (wheel
// events carry no dx). WDA devices swipe.
func (b *DeviceBackend) scrollGesture(ctx context.Context, udid string, region Frame, direction string) error {
	m := b.mirrorFor(udid)
	if m == nil {
		x1, y1, x2, y2 := swipeForDirection(region, direction)
		return b.swipeXY(ctx, udid, x1, y1, x2, y2)
	}
	if dy, ok := wheelDelta(region, direction); ok {
		err := m.Scroll(ctx, region.X+region.W/2, region.Y+region.H/2, dy)
		if err == nil {
			return nil
		}
		if !wheelFallbackable(err) {
			if gestureNoEffect(err) {
				return errGestureNoEffect
			}
			return mirrorFail("scroll", err)
		}
	}
	x1, y1, x2, y2 := swipeForDirection(region, direction)
	if err := m.Swipe(ctx, x1, y1, x2, y2, mirrorFlickDuration); err != nil {
		if gestureNoEffect(err) {
			return errGestureNoEffect
		}
		return mirrorFail("swipe", err)
	}
	return nil
}

// wheelDelta maps a scroll direction onto a wheel dy (positive scrolls
// content down, i.e. reveals content below the fold), sized like a swipe
// at the standard scroll fraction. Horizontal directions report !ok:
// wheel events carry no dx.
func wheelDelta(region Frame, direction string) (float64, bool) {
	d := region.H * scrollSwipeFraction
	switch direction {
	case "down":
		return d, true
	case "up":
		return -d, true
	}
	return 0, false
}

// wheelFallbackable reports whether a failed wheel scroll is worth
// retrying as a flick. Session-state failures (mirroring not running,
// no window, blocked interstitial, focus lost) need a human at the Mac
// or the phone — a flick would fail the same way, so they surface as-is.
// Posted-but-unproven failures must not flick either: the wheel events
// went out, so a second gesture on top of them could advance the list
// twice in one step (`input-not-delivered` can simply mean the phone
// rendered after the observation budget) — those surface as
// errGestureNoEffect for the scrolling loops to count as non-moves.
// `scroll-not-delivered` is the same case for a background wheel scroll
// the helper could not re-deliver focused under --background=on: those
// records went out too, so no flick on top of them either.
// A configuration refusal (`input-refused`) DOES get the flick: nothing
// was sent, and on a host where wheel scroll needs the focused path that
// --background=on forbids, the flick is a plain mouse drag the
// background path delivers — the only way lists scroll at all there.
// Anything else (an older mirrord without the /v1/scroll route,
// a generic failure) gets the flick too.
func wheelFallbackable(err error) bool {
	var ue *mirror.UnverifiedError
	if errors.As(err, &ue) {
		return false
	}
	var me *mirror.Error
	if errors.As(err, &me) {
		switch me.Code {
		case "not-running", "no-window", "blocked", "focus-lost",
			"input-not-delivered", "scroll-not-delivered":
			return false
		}
	}
	return true
}
