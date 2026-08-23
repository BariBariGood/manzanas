package actions

import (
	"context"
	"errors"
	"testing"
)

// fakeListDriver serves a scripted sequence of observations: index 0
// before any gesture, advancing one step per scrollGesture (clamped at
// the last screen — a list that stopped scrolling keeps reporting the
// same content, like a real end-of-list).
type fakeListDriver struct {
	seq      []observation
	i        int
	gestures []string
}

func (f *fakeListDriver) observeOnce(context.Context, string, bool) (observation, error) {
	i := f.i
	if i >= len(f.seq) {
		i = len(f.seq) - 1
	}
	return f.seq[i], nil
}

func (f *fakeListDriver) tapXY(context.Context, string, float64, float64) error { return nil }

func (f *fakeListDriver) typeInto(_ context.Context, _, text string, _ typeOpts) (int, error) {
	return len([]rune(text)), nil
}

func (f *fakeListDriver) scrollGesture(_ context.Context, _ string, _ Frame, direction string) error {
	f.gestures = append(f.gestures, direction)
	f.i++
	return nil
}

// ocrScreen builds an OCR-shaped observation (flat Text leaves, like the
// mirror backend produces) with rows spread down a 600x1300 viewport,
// clear of the volatile bands.
func ocrScreen(rows ...string) observation {
	nodes := make([]*Node, 0, len(rows))
	for i, r := range rows {
		nodes = append(nodes, &Node{
			Role:  "Text",
			Label: r,
			Frame: &Frame{X: 40, Y: float64(150 + i*120), W: 300, H: 40},
		})
	}
	return observation{nodes: nodes, viewport: &Frame{W: 600, H: 1300}}
}

func fastUntil(extra map[string]any) map[string]any {
	p := map[string]any{"timeout_ms": 2000.0, "interval_ms": 10.0}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func TestScrollUntilFindsTextAfterScrolling(t *testing.T) {
	d := &fakeListDriver{seq: []observation{
		ocrScreen("Alpha", "Bravo", "Charlie", "Delta"),
		ocrScreen("Echo", "Foxtrot", "Golf", "Hotel"),
		ocrScreen("India", "Juliet", "Kilo", "Target Row"),
	}}
	res, err := elemScrollUntil(context.Background(), d, "UDID",
		fastUntil(map[string]any{"text": "Target Row"}))
	if err != nil {
		t.Fatalf("scroll_until: %v", err)
	}
	if res["found"] != true || res["scrolls"] != 2 || res["end_of_list"] != false {
		t.Fatalf("res = %v", res)
	}
	el, ok := res["element"].(*Node)
	if !ok || el.Label != "Target Row" {
		t.Fatalf("element = %#v", res["element"])
	}
	history, _ := res["moved_history"].([]float64)
	if len(history) != 2 {
		t.Fatalf("moved_history = %v, want 2 entries", history)
	}
	for _, j := range history {
		if j >= 0.6 {
			t.Fatalf("fully-replaced screens should read as moved (jaccard < 0.6), got %v", history)
		}
	}
	if len(d.gestures) != 2 || d.gestures[0] != "down" {
		t.Fatalf("gestures = %v", d.gestures)
	}
}

func TestScrollUntilAlreadyVisibleDoesNotGesture(t *testing.T) {
	d := &fakeListDriver{seq: []observation{ocrScreen("Alpha", "Target Row")}}
	res, err := elemScrollUntil(context.Background(), d, "UDID",
		fastUntil(map[string]any{"text": "target row"}))
	if err != nil {
		t.Fatalf("scroll_until: %v", err)
	}
	if res["found"] != true || res["scrolls"] != 0 || res["screens"] != 1 {
		t.Fatalf("res = %v", res)
	}
	if len(d.gestures) != 0 {
		t.Fatalf("gestures = %v, want none", d.gestures)
	}
}

func TestScrollUntilEndOfListIsSoftResult(t *testing.T) {
	// Two screens of content, then the list stops moving: two confirmed
	// non-moves end the loop with found:false, not an error.
	d := &fakeListDriver{seq: []observation{
		ocrScreen("Alpha", "Bravo"),
		ocrScreen("Charlie", "Delta"),
	}}
	res, err := elemScrollUntil(context.Background(), d, "UDID",
		fastUntil(map[string]any{"text": "Missing Row", "max_scrolls": 10.0}))
	if err != nil {
		t.Fatalf("scroll_until: %v", err)
	}
	if res["found"] != false || res["end_of_list"] != true {
		t.Fatalf("res = %v", res)
	}
	// 1 moving gesture + 2 confirmed non-moves.
	if res["scrolls"] != 3 {
		t.Fatalf("scrolls = %v, want 3", res["scrolls"])
	}
	history, _ := res["moved_history"].([]float64)
	if len(history) != 3 || history[0] >= 0.6 || history[1] != 1 || history[2] != 1 {
		t.Fatalf("moved_history = %v", history)
	}
}

func TestScrollUntilBounceReadsAsNonMove(t *testing.T) {
	// An overscroll bounce: most content identical, one row of change.
	// High jaccard (well over 0.6) must count as a non-move.
	d := &fakeListDriver{seq: []observation{
		ocrScreen("Alpha", "Bravo", "Charlie", "Delta", "Echo"),
		ocrScreen("Alpha", "Bravo", "Charlie", "Delta", "Foxtrot"),
		ocrScreen("Alpha", "Bravo", "Charlie", "Delta", "Foxtrot"),
	}}
	res, err := elemScrollUntil(context.Background(), d, "UDID",
		fastUntil(map[string]any{"text": "Missing"}))
	if err != nil {
		t.Fatalf("scroll_until: %v", err)
	}
	if res["end_of_list"] != true || res["found"] != false {
		t.Fatalf("res = %v", res)
	}
	history, _ := res["moved_history"].([]float64)
	if len(history) == 0 || history[0] < 0.6 {
		t.Fatalf("bounce should have jaccard >= 0.6: %v", history)
	}
}

func TestScrollUntilMovedJaccardIsTunable(t *testing.T) {
	// The same bounce sequence counts as movement under a low threshold.
	d := &fakeListDriver{seq: []observation{
		ocrScreen("Alpha", "Bravo", "Charlie", "Delta", "Echo"),
		ocrScreen("Alpha", "Bravo", "Charlie", "Delta", "Foxtrot"),
		ocrScreen("Alpha", "Bravo", "Charlie", "Delta", "Golf"),
		ocrScreen("Alpha", "Bravo", "Charlie", "Delta", "Target"),
	}}
	res, err := elemScrollUntil(context.Background(), d, "UDID",
		fastUntil(map[string]any{"text": "Target", "moved_jaccard": 0.95}))
	if err != nil {
		t.Fatalf("scroll_until: %v", err)
	}
	if res["found"] != true || res["end_of_list"] != false {
		t.Fatalf("res = %v", res)
	}
}

func TestScrollUntilMasksVolatileBands(t *testing.T) {
	// The only change between screens is inside the top volatile band (a
	// ticking clock): masked out, the sets are identical -> non-moves.
	clockA := &Node{Role: "Text", Label: "9:41", Frame: &Frame{X: 20, Y: 10, W: 60, H: 20}}
	clockB := &Node{Role: "Text", Label: "9:42", Frame: &Frame{X: 20, Y: 10, W: 60, H: 20}}
	row := &Node{Role: "Text", Label: "Static Row", Frame: &Frame{X: 40, Y: 400, W: 300, H: 40}}
	vp := &Frame{W: 600, H: 1300}
	d := &fakeListDriver{seq: []observation{
		{nodes: []*Node{clockA, row}, viewport: vp},
		{nodes: []*Node{clockB, row}, viewport: vp},
	}}
	res, err := elemScrollUntil(context.Background(), d, "UDID",
		fastUntil(map[string]any{"text": "Missing"}))
	if err != nil {
		t.Fatalf("scroll_until: %v", err)
	}
	if res["end_of_list"] != true {
		t.Fatalf("clock tick must not read as movement: %v", res)
	}
	history, _ := res["moved_history"].([]float64)
	for _, j := range history {
		if j != 1 {
			t.Fatalf("masked sets should be identical (jaccard 1): %v", history)
		}
	}
}

// blindAfterGestureDriver stops producing observations once a gesture
// has been issued, so the overall budget expires mid-settle.
type blindAfterGestureDriver struct {
	fakeListDriver
}

func (d *blindAfterGestureDriver) observeOnce(ctx context.Context, udid string, refresh bool) (observation, error) {
	if len(d.gestures) > 0 {
		return observation{}, errNotYet
	}
	return d.fakeListDriver.observeOnce(ctx, udid, refresh)
}

func TestScrollUntilBudgetExpiryMidSettleIsSoftResult(t *testing.T) {
	d := &blindAfterGestureDriver{fakeListDriver{seq: []observation{
		ocrScreen("Alpha", "Bravo"),
	}}}
	res, err := elemScrollUntil(context.Background(), d, "UDID",
		map[string]any{"text": "Missing", "timeout_ms": 60.0, "interval_ms": 10.0})
	if err != nil {
		t.Fatalf("budget expiry mid-settle must return the partial result, got %v", err)
	}
	if res["found"] != false || res["scrolls"] != 1 || res["end_of_list"] != false {
		t.Fatalf("res = %v", res)
	}
}

func TestScrollUntilRequiresText(t *testing.T) {
	d := &fakeListDriver{seq: []observation{ocrScreen("Alpha")}}
	for _, p := range []map[string]any{{}, {"text": ""}, {"text": 42}} {
		_, err := elemScrollUntil(context.Background(), d, "UDID", fastUntil(p))
		if e, ok := err.(*Error); !ok || e.Code != "bad_request" {
			t.Fatalf("%v: err = %v, want bad_request", p, err)
		}
	}
}

func TestScrollUntilRejectsBadThreshold(t *testing.T) {
	d := &fakeListDriver{seq: []observation{ocrScreen("Alpha")}}
	for _, v := range []any{0, -0.5, 1.5, "high"} {
		_, err := elemScrollUntil(context.Background(), d, "UDID",
			fastUntil(map[string]any{"text": "x", "moved_jaccard": v}))
		if e, ok := err.(*Error); !ok || e.Code != "bad_request" {
			t.Fatalf("moved_jaccard=%v: err = %v, want bad_request", v, err)
		}
	}
}

func TestScrollUntilDirectionIsHonored(t *testing.T) {
	d := &fakeListDriver{seq: []observation{
		ocrScreen("Alpha", "Bravo"),
		ocrScreen("Target", "Charlie"),
	}}
	res, err := elemScrollUntil(context.Background(), d, "UDID",
		fastUntil(map[string]any{"text": "Target", "direction": "up"}))
	if err != nil {
		t.Fatalf("scroll_until: %v", err)
	}
	if res["found"] != true || len(d.gestures) != 1 || d.gestures[0] != "up" {
		t.Fatalf("res = %v, gestures = %v", res, d.gestures)
	}
}

// Mirror gesture: wheel scroll first, honoring the exposed mirror scroll.
func TestMirrorScrollGestureUsesWheel(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	region := Frame{X: 0, Y: 0, W: 600, H: 1300}
	if err := b.scrollGesture(context.Background(), "DEV-UDID", region, "down"); err != nil {
		t.Fatalf("scrollGesture: %v", err)
	}
	if got := f.callList(); len(got) != 1 || got[0] != "POST /v1/scroll" {
		t.Fatalf("calls = %v, want [POST /v1/scroll]", got)
	}
	body := f.lastBody()
	// Positive dy reveals content below the fold, sized like a swipe at
	// the standard scroll fraction (0.4 * 1300 = 520), at region centre.
	if body["x"] != float64(300) || body["y"] != float64(650) || body["dy"] != float64(520) {
		t.Fatalf("scroll body = %v", body)
	}
}

func TestMirrorScrollGestureFlicksHorizontally(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	region := Frame{X: 0, Y: 0, W: 600, H: 1300}
	if err := b.scrollGesture(context.Background(), "DEV-UDID", region, "right"); err != nil {
		t.Fatalf("scrollGesture: %v", err)
	}
	if got := f.callList(); len(got) != 1 || got[0] != "POST /v1/swipe" {
		t.Fatalf("calls = %v, want [POST /v1/swipe]", got)
	}
	if d := f.lastBody()["duration_ms"]; d != float64(150) {
		t.Fatalf("flick duration_ms = %v, want 150", d)
	}
}

func TestMirrorScrollGestureFallsBackToFlick(t *testing.T) {
	// An older mirrord without /v1/scroll: generic failure -> flick.
	f, b, _ := testMirrorBackend(t)
	f.fail["/v1/scroll"] = mirrorFailure{status: 404, code: "", message: "no route"}
	region := Frame{X: 0, Y: 0, W: 600, H: 1300}
	if err := b.scrollGesture(context.Background(), "DEV-UDID", region, "down"); err != nil {
		t.Fatalf("scrollGesture: %v", err)
	}
	got := f.callList()
	if len(got) != 2 || got[0] != "POST /v1/scroll" || got[1] != "POST /v1/swipe" {
		t.Fatalf("calls = %v, want wheel then flick", got)
	}
}

func TestMirrorScrollGestureSessionErrorsDoNotFlick(t *testing.T) {
	// A blocked interstitial needs a human; a flick would fail the same
	// way, so the wheel failure surfaces as-is with no second gesture.
	f, b, _ := testMirrorBackend(t)
	f.fail["/v1/scroll"] = mirrorFailure{status: 503, code: "blocked",
		message: "iPhone in Use"}
	region := Frame{X: 0, Y: 0, W: 600, H: 1300}
	err := b.scrollGesture(context.Background(), "DEV-UDID", region, "down")
	if err == nil {
		t.Fatal("want error")
	}
	if ae, ok := err.(*Error); !ok || ae.Code != "unavailable" {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if got := f.callList(); len(got) != 1 {
		t.Fatalf("calls = %v, want wheel only", got)
	}
}

// Full mirror dispatch: scroll_until over the fake mirrord's OCR boxes
// carries the fidelity declaration.
func TestMirrorScrollUntilDeclaresFidelity(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Following", "confidence": 0.95, "x": 100, "y": 400, "w": 80, "h": 20},
	}
	res, err := deviceDispatch(t, b, "scroll_until", map[string]any{
		"text": "Following", "timeout_ms": 2000, "interval_ms": 10})
	if err != nil {
		t.Fatalf("scroll_until: %v", err)
	}
	if res.Result["backend"] != "mirror" || res.Result["fidelity"] != "ocr" {
		t.Fatalf("res = %+v", res.Result)
	}
	if res.Result["found"] != true || res.Result["scrolls"] != 0 {
		t.Fatalf("res = %+v", res.Result)
	}
}

// noEffectListDriver reports every gesture as delivered-but-verified
// no-change, the shape mirrord's per-action pixel verification gives a
// scroll at the end of a list. The observation sequence still advances
// (like fakeListDriver), modelling a helper verdict that missed a late
// render.
type noEffectListDriver struct{ fakeListDriver }

func (d *noEffectListDriver) scrollGesture(_ context.Context, _ string, _ Frame, direction string) error {
	d.gestures = append(d.gestures, direction)
	d.i++
	return errGestureNoEffect
}

// A pixel-verified no-change gesture on a truly static screen is not
// fatal: the loop re-observes, the Jaccard check confirms the non-move,
// and two of them reach the documented soft end-of-list result.
func TestScrollUntilVerifiedNoEffectGestureFeedsEndOfList(t *testing.T) {
	d := &noEffectListDriver{fakeListDriver{seq: []observation{
		ocrScreen("Alpha", "Bravo"),
	}}}
	res, err := elemScrollUntil(context.Background(), d, "UDID",
		fastUntil(map[string]any{"text": "Missing"}))
	if err != nil {
		t.Fatalf("scroll_until: %v", err)
	}
	if res["found"] != false || res["end_of_list"] != true || res["scrolls"] != 2 {
		t.Fatalf("res = %v", res)
	}
	history, _ := res["moved_history"].([]float64)
	if len(history) != 2 || history[0] != 1 || history[1] != 1 {
		t.Fatalf("moved_history = %v, want two confirmed non-moves", res["moved_history"])
	}
}

// The no-change verdict is not trusted blindly: input-not-delivered can
// mean the phone rendered after the helper's observation budget. The
// loop must still observe the post-gesture screen — a target that
// appeared despite the verdict is found, not reported end-of-list.
func TestScrollUntilNoEffectVerdictStillObserves(t *testing.T) {
	d := &noEffectListDriver{fakeListDriver{seq: []observation{
		ocrScreen("Alpha", "Bravo"),
		ocrScreen("Target", "Charlie"),
	}}}
	res, err := elemScrollUntil(context.Background(), d, "UDID",
		fastUntil(map[string]any{"text": "Target"}))
	if err != nil {
		t.Fatalf("scroll_until: %v", err)
	}
	if res["found"] != true || res["scrolls"] != 1 || res["end_of_list"] != false {
		t.Fatalf("res = %v, the screen that moved despite the verdict must be seen", res)
	}
}

// The end-of-list collision with per-action verification (#232 review):
// on a static screen the wheel scroll comes back input-not-delivered.
// That must feed the end-of-list counter and reach the documented soft
// result — never a flick on top (double-gesture risk) and never a 503.
func TestMirrorScrollUntilStaticScreenReachesEndOfList(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Row", "confidence": 0.9, "x": 100, "y": 300, "w": 80, "h": 24},
	}
	f.fail["/v1/scroll"] = mirrorFailure{status: 503, code: "input-not-delivered",
		message: "screen did not change"}
	res, err := deviceDispatch(t, b, "scroll_until", map[string]any{
		"text": "Missing", "timeout_ms": 2000, "interval_ms": 10})
	if err != nil {
		t.Fatalf("scroll_until on a static screen: %v", err)
	}
	if res.Result["found"] != false || res.Result["end_of_list"] != true || res.Result["scrolls"] != 2 {
		t.Fatalf("res = %+v", res.Result)
	}
	for _, c := range f.callList() {
		if c == "POST /v1/swipe" {
			t.Fatalf("calls = %v, a verified no-change wheel must not flick", f.callList())
		}
	}
}

// A background wheel scroll the helper could not re-deliver focused
// (scroll-not-delivered, --background=on) is the same verdict: the
// records went out, so it counts as a non-move and never flicks.
func TestMirrorScrollUntilScrollNotDeliveredReachesEndOfList(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Row", "confidence": 0.9, "x": 100, "y": 300, "w": 80, "h": 24},
	}
	f.fail["/v1/scroll"] = mirrorFailure{status: 503, code: "scroll-not-delivered",
		message: "records accepted, nothing changed; --background=on forbids the focused fallback"}
	res, err := deviceDispatch(t, b, "scroll_until", map[string]any{
		"text": "Missing", "timeout_ms": 2000, "interval_ms": 10})
	if err != nil {
		t.Fatalf("scroll_until on an undelivered background scroll: %v", err)
	}
	if res.Result["found"] != false || res.Result["end_of_list"] != true {
		t.Fatalf("res = %+v", res.Result)
	}
	for _, c := range f.callList() {
		if c == "POST /v1/swipe" {
			t.Fatalf("calls = %v, an unproven wheel must not flick", f.callList())
		}
	}
}

// An unverified wheel ack whose check ran (verified=false, a real
// pixel-delta verdict) is the same confirmed non-move.
func TestMirrorScrollGestureUnverifiedWheelIsNoEffect(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.verified = false
	region := Frame{X: 0, Y: 0, W: 600, H: 1300}
	err := b.scrollGesture(context.Background(), "DEV-UDID", region, "down")
	if !errors.Is(err, errGestureNoEffect) {
		t.Fatalf("err = %v, want errGestureNoEffect", err)
	}
	if got := f.callList(); len(got) != 1 || got[0] != "POST /v1/scroll" {
		t.Fatalf("calls = %v, want wheel only (no flick)", got)
	}
}

// verify=unavailable means nobody could look: the screen may well have
// moved, so it stays a hard error rather than a fake non-move.
func TestMirrorScrollGestureVerifyUnavailableStaysFatal(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.verified = false
	f.verify = "unavailable"
	region := Frame{X: 0, Y: 0, W: 600, H: 1300}
	err := b.scrollGesture(context.Background(), "DEV-UDID", region, "down")
	if err == nil || errors.Is(err, errGestureNoEffect) {
		t.Fatalf("err = %v, want a hard error", err)
	}
	if ae, ok := err.(*Error); !ok || ae.Code != "unavailable" {
		t.Fatalf("err = %v, want unavailable", err)
	}
}

// scroll_to_element hits the same collision through swipeXY: a static
// screen's verified no-change swipes must end in the documented timeout
// (target never appeared), not a 503 from the ack check.
func TestMirrorScrollToElementStaticScreenIsTimeout(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Row", "confidence": 0.9, "x": 100, "y": 300, "w": 80, "h": 24},
	}
	f.fail["/v1/swipe"] = mirrorFailure{status: 503, code: "input-not-delivered",
		message: "screen did not change"}
	_, err := deviceDispatch(t, b, "scroll_to_element", map[string]any{
		"label": "Missing", "timeout_ms": 2000, "interval_ms": 10})
	if err == nil {
		t.Fatal("want the documented scroll failure")
	}
	if ae, ok := err.(*Error); !ok || ae.Code != "timeout" {
		t.Fatalf("err = %v, want timeout (not an input error)", err)
	}
	swipes := 0
	for _, c := range f.callList() {
		if c == "POST /v1/swipe" {
			swipes++
		}
	}
	if swipes != 2 {
		t.Fatalf("swipes = %d, want the two confirming no-effect gestures", swipes)
	}
}

// noEffectScrollDriver: a scrollDriver whose every swipe comes back as a
// verified no-change verdict, while the observation sequence still
// advances (a verdict that keeps missing the render).
type noEffectScrollDriver struct {
	fakeListDriver
	swipes int
}

func (d *noEffectScrollDriver) swipeXY(context.Context, string, float64, float64, float64, float64) error {
	d.i++
	d.swipes++
	return errGestureNoEffect
}

// scroll_to_element must corroborate the no-change verdict against the
// observed screen: while the screen keeps changing the no-effect streak
// resets, so a fallible verdict cannot end the search after two swipes.
func TestScrollToElementNoEffectStreakResetsWhenScreenMoves(t *testing.T) {
	d := &noEffectScrollDriver{fakeListDriver: fakeListDriver{seq: []observation{
		ocrScreen("Alpha", "Bravo"),
		ocrScreen("Charlie", "Delta"),
		ocrScreen("Target", "Echo"),
	}}}
	res, err := elemScrollTo(context.Background(), d, "UDID",
		fastUntil(map[string]any{"label": "Target"}))
	if err != nil {
		t.Fatalf("scroll_to_element: %v", err)
	}
	if res["scrolls"] != 2 {
		t.Fatalf("res = %v, want the moving screens to keep the loop alive", res)
	}
}

// On a screen that really holds still, two no-effect swipes end in the
// documented failure without burning the whole max_scrolls budget.
func TestScrollToElementStaticNoEffectStopsAfterTwo(t *testing.T) {
	d := &noEffectScrollDriver{fakeListDriver: fakeListDriver{seq: []observation{
		ocrScreen("Alpha", "Bravo"),
	}}}
	_, err := elemScrollTo(context.Background(), d, "UDID",
		fastUntil(map[string]any{"label": "Missing"}))
	if err == nil {
		t.Fatal("want the documented scroll failure")
	}
	if ae, ok := err.(*Error); !ok || ae.Code != "timeout" {
		t.Fatalf("err = %v, want timeout", err)
	}
	if d.swipes != 2 {
		t.Fatalf("swipes = %d, want exactly the two confirming gestures", d.swipes)
	}
}
