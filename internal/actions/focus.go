package actions

import (
	"context"
	"errors"
	"time"
)

// Focus-guard budget: how long the guard polls for keyboard evidence
// before concluding no text field has focus. Variables so tests can
// shrink them.
// The budget must cover several observes even on the cold path (an AXe
// spawn per poll, ~1-3 s each, ~3-5 s for the first observe after an app
// launch).
var (
	focusWaitTimeout  = 10 * time.Second
	focusWaitInterval = 250 * time.Millisecond
)

// requireFocusedField polls the a11y tree until it shows evidence that a
// text field has keyboard focus (the on-screen keyboard's Key/Keyboard
// elements), guarding the typing path against stray keystrokes landing
// outside any field — where the simulator forwards them to whatever else
// is listening (a hardware-keyboard 'r' reaching Metro reloads the app).
//
// The software keyboard is the observable focus signal AXe exposes; a
// simulator with "Connect Hardware Keyboard" active hides it, so such
// setups must not set require_focus.
//
// refresh carries the action's `refresh` payload flag so the guard
// observes with the same freshness semantics as the rest of the action.
func requireFocusedField(ctx context.Context, d elementDriver, udid string, refresh bool) error {
	_, _, err := pollUntil(ctx, focusWaitTimeout, focusWaitInterval, func(ctx context.Context) error {
		obs, err := d.observeOnce(ctx, udid, refresh)
		if err != nil {
			return err
		}
		if keyboardVisible(obs.nodes) {
			return nil
		}
		return errNotYet
	})
	if err != nil {
		if errors.Is(err, errNotYet) || errors.Is(err, context.DeadlineExceeded) {
			return focusRequired("no focused text field: the on-screen keyboard is not visible after %s; tap a text field first (or pass require_focus:false if the simulator has Connect Hardware Keyboard active)", focusWaitTimeout)
		}
		return err
	}
	return nil
}

// keyboardVisible reports whether the compacted tree contains on-screen
// keyboard elements (role Key or Keyboard) — the observable evidence that
// a text field has keyboard focus.
func keyboardVisible(nodes []*Node) bool {
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Role == "Key" || n.Role == "Keyboard" {
			return true
		}
		if keyboardVisible(n.Children) {
			return true
		}
	}
	return false
}
