package actions

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
)

// fakeMirrord is an in-process mirrord helper served over a unix socket,
// so the real mirror.Client transport is exercised on Linux CI.
type fakeMirrord struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	calls []string         // "METHOD /path" per request
	body  []map[string]any // decoded JSON bodies (nil for GET)

	// boxes served by /v1/ocr.
	boxes []map[string]any
	// fail maps a path to a canned error response {status, code, message}.
	fail map[string]mirrorFailure
	// failPost is fail, but only for POST requests — GET still succeeds
	// (models a pre-options helper with no POST /v1/ocr route).
	failPost map[string]mirrorFailure

	// input acknowledgement served by the input routes, mirroring the
	// helper's {ok, verified, verify, mode} shape.
	inputMode string
	verify    string
	verified  bool
	note      string
	// durationMS, when non-zero, is echoed as the ack's duration_ms —
	// models a modern helper reporting the (possibly clamped) duration
	// it actually ran. Zero echoes the requested value, like the real
	// helper does for in-range requests.
	durationMS float64

	// png, when set, replaces the default 4x4 screenshot (pngW/pngH are
	// its pixel dimensions).
	png        []byte
	pngW, pngH int
}

type mirrorFailure struct {
	status  int
	code    string
	message string
}

func newFakeMirrord(t *testing.T) (*fakeMirrord, *mirror.Client) {
	t.Helper()
	f := &fakeMirrord{
		t: t, fail: map[string]mirrorFailure{}, failPost: map[string]mirrorFailure{},
		// A healthy modern helper: focus-free input, verified by capture
		// delta.
		inputMode: "background", verify: "pixel-delta", verified: true,
	}
	// Not t.TempDir(): its per-test-name path blows past macOS's 104-byte
	// sockaddr_un limit.
	dir, err := os.MkdirTemp("", "m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = &httptest.Server{
		Listener: ln,
		Config:   &http.Server{Handler: http.HandlerFunc(f.handle)},
	}
	f.srv.Start()
	t.Cleanup(f.srv.Close)
	return f, mirror.New(sock)
}

func (f *fakeMirrord) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	f.body = append(f.body, body)
	fail, failing := f.fail[r.URL.Path]
	if !failing && r.Method == http.MethodPost {
		fail, failing = f.failPost[r.URL.Path]
	}
	boxes := f.boxes
	shotPNG, shotW, shotH := f.png, f.pngW, f.pngH
	ack := map[string]any{"ok": true}
	if f.inputMode != "" {
		ack["mode"] = f.inputMode
		ack["verified"] = f.verified
		ack["verify"] = f.verify
	}
	if f.note != "" {
		ack["note"] = f.note
	}
	if f.durationMS != 0 {
		ack["duration_ms"] = f.durationMS
	} else if d, ok := body["duration_ms"]; ok {
		ack["duration_ms"] = d
	}
	status := map[string]any{
		"state": "ready", "frontmost": true,
		"background": f.inputMode == "background", "input_mode": f.inputMode,
		"background_scroll": f.inputMode == "background",
		"capture":           "sck", "verify": f.verify,
		"window": map[string]any{"window_id": 4242, "x": 100, "y": 50, "w": 320, "h": 700},
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if failing {
		w.WriteHeader(fail.status)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": fail.code, "error": fail.message})
		return
	}
	switch r.URL.Path {
	case "/v1/status":
		_ = json.NewEncoder(w).Encode(status)
	case "/v1/doctor":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                true,
			"state":             "ready",
			"capture":           "sck",
			"verify":            status["verify"],
			"input_mode":        status["input_mode"],
			"background":        status["background"],
			"background_scroll": status["background_scroll"],
			"checks": []map[string]any{
				{"name": "background_input", "ok": true, "detail": "event record accepted"},
				{"name": "capture", "ok": true, "detail": "sck: 320x700"},
			},
		})
	case "/v1/screenshot":
		pngBytes, imgW, imgH := tinyPNGBytes(f.t), 4, 4
		if shotPNG != nil {
			pngBytes, imgW, imgH = shotPNG, shotW, shotH
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"png_base64": base64.StdEncoding.EncodeToString(pngBytes),
			"img_w":      imgW, "img_h": imgH, "scale": 2.0,
		})
	case "/v1/ocr":
		if boxes == nil {
			boxes = []map[string]any{}
		}
		out := map[string]any{"boxes": boxes, "img_w": 600, "img_h": 1300, "scale": 2.0}
		// POST carries options; echo the applied metadata like mirrord does.
		if r.Method == http.MethodPost {
			out["served_scale"] = 0.5
			if lvl, ok := body["level"]; ok {
				out["level"] = lvl
			}
			if rc, ok := body["rect"]; ok {
				out["rect"] = rc
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	default:
		_ = json.NewEncoder(w).Encode(ack)
	}
}

func tinyPNGBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (f *fakeMirrord) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeMirrord) lastBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.body) == 0 {
		return nil
	}
	return f.body[len(f.body)-1]
}

// testMirrorBackend builds a DeviceBackend with DEV-UDID mirror-backed.
func testMirrorBackend(t *testing.T) (*fakeMirrord, *DeviceBackend, *fakeRunner) {
	t.Helper()
	f, c := newFakeMirrord(t)
	r := newFakeRunner()
	b := NewDevice(WithDeviceRunner(r), WithDeviceMirror(map[string]*mirror.Client{"DEV-UDID": c}),
		WithDeviceMirrorInput(map[string]bool{"DEV-UDID": true}))
	return f, b, r
}

func TestMirrorStatusWindowID(t *testing.T) {
	_, c := newFakeMirrord(t)
	s, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.State != "ready" || !s.Frontmost {
		t.Fatalf("status = %+v", s)
	}
	if s.Window == nil || s.Window.ID != 4242 {
		t.Fatalf("window = %+v, want window_id 4242", s.Window)
	}
	if s.Window.X != 100 || s.Window.Y != 50 || s.Window.W != 320 || s.Window.H != 700 {
		t.Fatalf("window bounds = %+v", s.Window)
	}
}

func TestMirrorStatusCapabilities(t *testing.T) {
	_, c := newFakeMirrord(t)
	s, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Background || s.InputMode != "background" {
		t.Fatalf("status = %+v, want focus-free input", s)
	}
	if s.Capture != "sck" || s.Verify != "pixel-delta" {
		t.Fatalf("status = %+v, want sck capture and pixel-delta verification", s)
	}
	if !s.BackgroundScroll {
		t.Fatalf("status = %+v, want background scroll capability", s)
	}
}

func TestMirrorDoctor(t *testing.T) {
	_, c := newFakeMirrord(t)
	r, err := c.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK || !r.Background || r.InputMode != "background" || r.Capture != "sck" {
		t.Fatalf("doctor = %+v", r)
	}
	if !r.BackgroundScroll {
		t.Fatalf("doctor = %+v, want background scroll capability", r)
	}
	if len(r.Checks) != 2 || r.Checks[0].Name != "background_input" || !r.Checks[0].OK {
		t.Fatalf("doctor checks = %+v", r.Checks)
	}
}

// Every mirror input action journals the mode that delivered it and the
// verification evidence, so a journal reader can tell a focus-free,
// verified tap from a focus-stealing or unverified one.
func TestMirrorInputJournalsModeAndVerification(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.inputMode = "focused"
	f.note = "scroll has no background record; used the focused path"
	for _, tc := range []struct {
		kind    string
		payload map[string]any
	}{
		{"tap", map[string]any{"x": 10, "y": 20}},
		{"tap", map[string]any{"x": 10, "y": 20, "duration_ms": 700}},
		{"swipe", map[string]any{"start_x": 1, "start_y": 2, "end_x": 3, "end_y": 4}},
		{"scroll", map[string]any{"x": 5, "y": 6, "dy": -20}},
		{"type", map[string]any{"text": "hi"}},
		{"button", map[string]any{"name": "home"}},
	} {
		res, err := deviceDispatch(t, b, tc.kind, tc.payload)
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		if res.Result["input_mode"] != "focused" {
			t.Fatalf("%s result = %+v, want input_mode focused", tc.kind, res.Result)
		}
		if res.Result["verified"] != true || res.Result["verify"] != "pixel-delta" {
			t.Fatalf("%s result = %+v, want verified pixel-delta", tc.kind, res.Result)
		}
		if res.Result["input_note"] != f.note {
			t.Fatalf("%s result = %+v, want the helper's note", tc.kind, res.Result)
		}
	}
}

// A type delivered as "focused-degrade" (the helper briefly activated
// the window because iOS routes key events into text input only while
// iPhone Mirroring is frontmost) surfaces the degraded mode and the
// helper's note on the result instead of failing or hiding the focus
// grab.
func TestMirrorTypeFocusedDegradeSurfacesModeAndNote(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.inputMode = "focused-degrade"
	f.note = "type ran on the focused path; Terminal took focus back"
	res, err := deviceDispatch(t, b, "type", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["input_mode"] != "focused-degrade" {
		t.Fatalf("result = %+v, want input_mode focused-degrade", res.Result)
	}
	if res.Result["input_note"] != f.note {
		t.Fatalf("result = %+v, want the helper's degrade note", res.Result)
	}
	if res.Result["verified"] != true || res.Result["verify"] != "pixel-delta" {
		t.Fatalf("result = %+v, want the helper's verified verdict", res.Result)
	}
}

// verify:true on a mirror-backed device defers to the helper's own
// verification: the daemon-level pixel wrapper must not clobber the
// helper's verified verdict or spend extra captures on the serialized
// helper.
func TestMirrorVerifiedTapDefersToHelper(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	res, err := deviceDispatch(t, b, "tap", map[string]any{"x": 10, "y": 20, "verify": true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["verified"] != true || res.Result["verify"] != "pixel-delta" {
		t.Fatalf("res = %+v, want the helper's verified pixel-delta verdict", res.Result)
	}
	if _, ok := res.Result["pixel_delta"]; ok {
		t.Fatalf("res = %+v, want no daemon-level pixel_delta (helper owns verification)", res.Result)
	}
	if got := f.callList(); len(got) != 1 || got[0] != "POST /v1/tap" {
		t.Fatalf("calls = %v, want a single tap with no pre/post screenshots", got)
	}
}

// A helper whose own verification is off cannot answer verify:true; the
// caller must get an explicit verify_error rather than a plain success
// with no verification evidence at all.
func TestMirrorVerifiedTapHelperVerifyOff(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.verify = "off"
	f.verified = false
	res, err := deviceDispatch(t, b, "tap", map[string]any{"x": 10, "y": 20, "verify": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Result["verify_error"]; !ok {
		t.Fatalf("res = %+v, want a verify_error when the helper cannot verify", res.Result)
	}
	if res.Result["verified"] == true {
		t.Fatalf("res = %+v, want no affirmative verified verdict", res.Result)
	}
}

// An action the helper posted but could not see land is a failure: the UI
// did not change, so reporting success would make callers act on a screen
// state that never happened.
func TestMirrorUnverifiedInputFails(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.verified = false
	for _, tc := range []struct {
		verify string
		want   string
	}{
		{"pixel-delta", "unavailable"},
		{"unavailable", "unavailable"},
		// Verification switched off at the helper makes no claim, so the
		// action stands (and says so in the journal).
		{"off", ""},
		// A helper predating verification says nothing at all.
		{"", ""},
	} {
		f.verify = tc.verify
		res, err := deviceDispatch(t, b, "tap", map[string]any{"x": 1, "y": 2})
		if tc.want == "" {
			if err != nil {
				t.Fatalf("verify=%q: %v", tc.verify, err)
			}
			if res.Result["verified"] == true {
				t.Fatalf("verify=%q result = %+v, must not claim verification", tc.verify, res.Result)
			}
			continue
		}
		if got := actionCode(t, err); got != tc.want {
			t.Fatalf("verify=%q: code %q, want %q", tc.verify, got, tc.want)
		}
		// A check that never ran must not claim the screen is unchanged
		// (a retry on that advice can duplicate a tap or re-type text);
		// only a completed pixel-delta check licenses a plain retry.
		if tc.verify == "unavailable" && !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("verify=unavailable error %q must report delivery as unknown", err)
		}
		if tc.verify == "pixel-delta" && !strings.Contains(err.Error(), "did not change") {
			t.Fatalf("verify=pixel-delta error %q must report the screen as unchanged", err)
		}
	}
}

// input-not-delivered is the helper's typed "posted, nothing happened"
// failure; it must surface as a retryable unavailable, not a caller bug.
func TestMirrorInputNotDeliveredMapsToUnavailable(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.fail["/v1/tap"] = mirrorFailure{status: 503, code: "input-not-delivered",
		message: "background input produced no change on the phone after 2 attempts"}
	_, err := deviceDispatch(t, b, "tap", map[string]any{"x": 1, "y": 2})
	if got := actionCode(t, err); got != "unavailable" {
		t.Fatalf("code = %q (%v)", got, err)
	}
	if !strings.Contains(err.Error(), "posted but not delivered") {
		t.Fatalf("error = %v, want an actionable message", err)
	}
}

// input-refused means the helper sent nothing (a --background=on
// configuration refusal); the daemon must not claim events were posted
// or advise a plain retry that cannot succeed.
func TestMirrorInputRefusedMapsToConfigurationError(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.fail["/v1/tap"] = mirrorFailure{status: 503, code: "input-refused",
		message: "background input is unavailable and --background=on forbids the focused fallback; no input was sent"}
	_, err := deviceDispatch(t, b, "tap", map[string]any{"x": 1, "y": 2})
	if got := actionCode(t, err); got != "unavailable" {
		t.Fatalf("code = %q (%v)", got, err)
	}
	if !strings.Contains(err.Error(), "nothing was sent") ||
		!strings.Contains(err.Error(), "--background") {
		t.Fatalf("error = %v, want a configuration message that does not claim events were posted", err)
	}
}

// With the fallback:"swipe" opt-in, a wheel scroll the helper refused
// without sending anything (input-refused under --background=on, or
// focus-lost on the focused path) is delivered as the equivalent drag,
// and the result says so honestly.
func TestMirrorScrollSwipeFallbackOnRefusal(t *testing.T) {
	for _, tc := range []mirrorFailure{
		{status: 503, code: "input-refused",
			message: "scroll has no background record and --background=on forbids the focused path; no input was sent"},
		{status: 503, code: "focus-lost",
			message: "iPhone Mirroring could not be brought frontmost (System Settings holds focus); input was not sent"},
	} {
		f, b, _ := testMirrorBackend(t)
		f.fail["/v1/scroll"] = tc
		f.png, f.pngW, f.pngH = tinyPNGBytes(t), 640, 1400
		res, err := deviceDispatch(t, b, "scroll",
			map[string]any{"x": 100, "y": 400, "dy": 200, "fallback": "swipe"})
		if err != nil {
			t.Fatalf("%s: %v", tc.code, err)
		}
		// One capture learns the bounds the drag must stay inside.
		if got := f.callList(); len(got) != 3 || got[0] != "POST /v1/scroll" ||
			got[1] != "GET /v1/screenshot" || got[2] != "POST /v1/swipe" {
			t.Fatalf("%s: calls = %v, want wheel, capture, swipe", tc.code, got)
		}
		// The equivalent drag: centered on (x, y), covering dy pixels,
		// positive dy dragging the finger up, at the flick duration.
		body := f.lastBody()
		if body["x1"] != 100.0 || body["y1"] != 500.0 ||
			body["x2"] != 100.0 || body["y2"] != 300.0 {
			t.Fatalf("%s: swipe body = %v, want (100,500)->(100,300)", tc.code, body)
		}
		if body["duration_ms"] != float64(mirrorFlickDuration.Milliseconds()) {
			t.Fatalf("%s: duration_ms = %v, want %d", tc.code, body["duration_ms"],
				mirrorFlickDuration.Milliseconds())
		}
		// Honest result metadata: the journal shows a swipe delivered it.
		if res.Result["method"] != "swipe-fallback" {
			t.Fatalf("%s: result = %+v, want method swipe-fallback", tc.code, res.Result)
		}
		if res.Result["swipe_start_y"] != 500.0 || res.Result["swipe_end_y"] != 300.0 {
			t.Fatalf("%s: result = %+v, want the swipe geometry stamped", tc.code, res.Result)
		}
		note, _ := res.Result["note"].(string)
		if !strings.Contains(note, tc.code) || !strings.Contains(note, "swipe") {
			t.Fatalf("%s: note = %q, want the refusal named", tc.code, note)
		}
		// The swipe's ack fields carry through (the fake acks verified
		// background input).
		if res.Result["input_mode"] != "background" || res.Result["verified"] != true {
			t.Fatalf("%s: result = %+v, want the swipe's ack fields", tc.code, res.Result)
		}
	}
}

// A negative dy (content up) drags the finger down, and a drag that
// would leave the capture is shifted back inside it (length preserved)
// near either edge — the helper bounds-checks both drag endpoints, so a
// valid scroll point must never turn into a rejected swipe.
func TestMirrorScrollSwipeFallbackGeometry(t *testing.T) {
	for _, tc := range []struct {
		x, y, dy float64
		y1, y2   float64
	}{
		{100, 400, -200, 300, 500},    // content up: finger drags down
		{100, 50, 200, 200, 0},        // top edge: shifted down, length kept
		{100, 1350, -200, 1199, 1399}, // bottom edge: shifted up, length kept
	} {
		f, b, _ := testMirrorBackend(t)
		f.fail["/v1/scroll"] = mirrorFailure{status: 503, code: "input-refused",
			message: "no input was sent"}
		f.png, f.pngW, f.pngH = tinyPNGBytes(t), 640, 1400
		if _, err := deviceDispatch(t, b, "scroll",
			map[string]any{"x": tc.x, "y": tc.y, "dy": tc.dy, "fallback": "swipe"}); err != nil {
			t.Fatalf("y=%v dy=%v: %v", tc.y, tc.dy, err)
		}
		body := f.lastBody()
		if body["y1"] != tc.y1 || body["y2"] != tc.y2 {
			t.Fatalf("y=%v dy=%v: swipe body = %v, want y1=%v y2=%v", tc.y, tc.dy, body, tc.y1, tc.y2)
		}
	}
}

// A scroll point outside the capture gets the documented bad_request on
// the fallback path too (the helper refuses scroll before its own bounds
// check ran), instead of being clamped onto a gesture somewhere else.
func TestMirrorScrollSwipeFallbackOutOfCapturePoint(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.fail["/v1/scroll"] = mirrorFailure{status: 503, code: "input-refused",
		message: "no input was sent"}
	f.png, f.pngW, f.pngH = tinyPNGBytes(t), 640, 1400
	_, err := deviceDispatch(t, b, "scroll",
		map[string]any{"x": 100, "y": 2000, "dy": 200, "fallback": "swipe"})
	if got := actionCode(t, err); got != "bad_request" {
		t.Fatalf("error code = %q, want bad_request", got)
	}
	for _, call := range f.callList() {
		if call == "POST /v1/swipe" {
			t.Fatal("swipe was posted for an out-of-capture scroll point")
		}
	}
}

// A capture failure on the fallback path degrades to the 0-side clamp
// only: the swipe is still attempted rather than the fallback dying on
// the bounds lookup.
func TestMirrorScrollSwipeFallbackCaptureUnavailable(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.fail["/v1/scroll"] = mirrorFailure{status: 503, code: "input-refused",
		message: "no input was sent"}
	f.fail["/v1/screenshot"] = mirrorFailure{status: 503, code: "capture-failed",
		message: "no capture"}
	if _, err := deviceDispatch(t, b, "scroll",
		map[string]any{"x": 100, "y": 400, "dy": 200, "fallback": "swipe"}); err != nil {
		t.Fatal(err)
	}
	body := f.lastBody()
	if body["y1"] != 500.0 || body["y2"] != 300.0 {
		t.Fatalf("swipe body = %v, want y1=500 y2=300", body)
	}
}

// Without the opt-in the refusal surfaces as before: no second gesture,
// and the configuration error reaches the caller unchanged.
func TestMirrorScrollNoFallbackWithoutOptIn(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.fail["/v1/scroll"] = mirrorFailure{status: 503, code: "input-refused",
		message: "no input was sent"}
	_, err := deviceDispatch(t, b, "scroll", map[string]any{"x": 100, "y": 400, "dy": 200})
	if got := actionCode(t, err); got != "unavailable" {
		t.Fatalf("code = %q (%v)", got, err)
	}
	if got := f.callList(); len(got) != 1 || got[0] != "POST /v1/scroll" {
		t.Fatalf("calls = %v, want the wheel scroll only", got)
	}
}

// The fallback covers refusals only: a failure where events may have
// reached the phone (or where a swipe would fail the same way) must not
// grow a second gesture on top.
func TestMirrorScrollFallbackNotForOtherErrors(t *testing.T) {
	for _, tc := range []mirrorFailure{
		{status: 503, code: "input-not-delivered", message: "posted, no change"},
		{status: 503, code: "scroll-not-delivered", message: "records accepted, no change"},
		{status: 503, code: "blocked", message: "iPhone in Use"},
	} {
		f, b, _ := testMirrorBackend(t)
		f.fail["/v1/scroll"] = tc
		_, err := deviceDispatch(t, b, "scroll",
			map[string]any{"x": 100, "y": 400, "dy": 200, "fallback": "swipe"})
		if err == nil {
			t.Fatalf("%s: want an error", tc.code)
		}
		if got := f.callList(); len(got) != 1 || got[0] != "POST /v1/scroll" {
			t.Fatalf("%s: calls = %v, want no fallback swipe", tc.code, got)
		}
	}
}

// fallback accepts only "swipe"; anything else is a caller bug caught
// before any input is posted.
func TestMirrorScrollFallbackRejectsBadValue(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	for _, v := range []any{"flick", true, 1} {
		_, err := deviceDispatch(t, b, "scroll",
			map[string]any{"x": 100, "y": 400, "dy": 200, "fallback": v})
		if got := actionCode(t, err); got != "bad_request" {
			t.Fatalf("fallback=%v: code = %q (%v)", v, got, err)
		}
	}
	if got := f.callList(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

// The error-only client wrappers (used by the element-composite drivers)
// must carry the evidence contract too: an unverified ack surfaces as a
// typed error, and mirrorFail words it like mirrorAckCheck would.
func TestMirrorErrorOnlyWrappersFailUnverified(t *testing.T) {
	f, c := newFakeMirrord(t)
	f.verified = false
	for _, tc := range []struct {
		verify string
		want   string
	}{
		{"pixel-delta", "did not change"},
		{"unavailable", "unknown"},
	} {
		f.verify = tc.verify
		err := c.Tap(context.Background(), 1, 2)
		var ue *mirror.UnverifiedError
		if !errors.As(err, &ue) {
			t.Fatalf("verify=%q: err = %v, want UnverifiedError", tc.verify, err)
		}
		mapped := mirrorFail("tap", err)
		if !strings.Contains(mapped.Error(), tc.want) {
			t.Fatalf("verify=%q: mapped error %q, want %q", tc.verify, mapped, tc.want)
		}
	}
	// verify:"off" makes no claim, so the wrapper succeeds.
	f.verify = "off"
	if err := c.Tap(context.Background(), 1, 2); err != nil {
		t.Fatalf("verify=off: %v", err)
	}
}

func TestMirrorTap(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	res, err := deviceDispatch(t, b, "tap", map[string]any{"x": 100, "y": 200})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["backend"] != "mirror" {
		t.Fatalf("res = %+v, want backend mirror", res.Result)
	}
	if got := f.callList(); len(got) != 1 || got[0] != "POST /v1/tap" {
		t.Fatalf("calls = %v", got)
	}
	body := f.lastBody()
	if body["x"] != float64(100) || body["y"] != float64(200) {
		t.Fatalf("tap body = %v", body)
	}
}

func TestMirrorLongPress(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	res, err := deviceDispatch(t, b, "tap", map[string]any{"x": 100, "y": 200, "duration_ms": 800})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["backend"] != "mirror" || res.Result["duration_ms"] != float64(800) {
		t.Fatalf("res = %+v", res.Result)
	}
	if got := f.callList(); len(got) != 1 || got[0] != "POST /v1/tap" {
		t.Fatalf("calls = %v", got)
	}
	body := f.lastBody()
	if body["x"] != float64(100) || body["y"] != float64(200) || body["duration_ms"] != float64(800) {
		t.Fatalf("tap body = %v", body)
	}
}

// The helper's ack is the authority on the duration that ran: when it
// echoes a clamped value, the action result reports that value, not the
// requested one.
func TestMirrorLongPressReportsHelperClampedDuration(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.durationMS = 500
	res, err := deviceDispatch(t, b, "tap", map[string]any{"x": 1, "y": 2, "duration_ms": 800})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["duration_ms"] != float64(500) {
		t.Fatalf("res = %+v, want duration_ms 500", res.Result)
	}
}

func TestMirrorLongPressRejectsBadDuration(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	for _, d := range []any{0, -1, 10001, "long"} {
		_, err := deviceDispatch(t, b, "tap", map[string]any{"x": 1, "y": 2, "duration_ms": d})
		if actionCode(t, err) != "bad_request" {
			t.Fatalf("duration_ms=%v: %v", d, err)
		}
	}
	// Rejected before any helper call.
	if got := f.callList(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

func TestMirrorScroll(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	res, err := deviceDispatch(t, b, "scroll", map[string]any{"x": 300, "y": 650, "dy": -120})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["backend"] != "mirror" || res.Result["dy"] != float64(-120) {
		t.Fatalf("res = %+v", res.Result)
	}
	if got := f.callList(); len(got) != 1 || got[0] != "POST /v1/scroll" {
		t.Fatalf("calls = %v", got)
	}
	body := f.lastBody()
	if body["x"] != float64(300) || body["y"] != float64(650) || body["dy"] != float64(-120) {
		t.Fatalf("scroll body = %v", body)
	}
}

func TestMirrorScrollRejectsBadPayload(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	for _, p := range []map[string]any{
		{"x": 1, "y": 2},               // missing dy
		{"x": 1, "y": 2, "dy": 0},      // zero dy
		{"x": 1, "y": 2, "dy": "down"}, // non-numeric dy
		{"x": -1, "y": 2, "dy": 10},    // negative coordinate
	} {
		_, err := deviceDispatch(t, b, "scroll", p)
		if actionCode(t, err) != "bad_request" {
			t.Fatalf("%v: %v", p, err)
		}
	}
	if got := f.callList(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

func TestMirrorObserveInteractiveOnlyReturnsAllNodes(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Settings", "x": 10.0, "y": 20.0, "w": 100.0, "h": 30.0, "confidence": 0.9},
	}
	res, err := deviceDispatch(t, b, "observe", map[string]any{"interactive_only": true})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	tree, ok := res.Result["tree"].([]*Node)
	if !ok || len(tree) != 1 {
		t.Fatalf("tree = %#v, want the full 1-node tree", res.Result["tree"])
	}
	note, _ := res.Result["note"].(string)
	if !strings.Contains(note, "interactive_only is a no-op") {
		t.Fatalf("note = %q, want interactive_only honesty note", note)
	}
}

func TestMirrorSwipe(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	res, err := deviceDispatch(t, b, "swipe", map[string]any{
		"start_x": 10, "start_y": 20, "end_x": 30, "end_y": 40, "duration_seconds": 0.25})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["backend"] != "mirror" {
		t.Fatalf("res = %+v", res.Result)
	}
	if got := f.callList(); len(got) != 1 || got[0] != "POST /v1/swipe" {
		t.Fatalf("calls = %v", got)
	}
	if d := f.lastBody()["duration_ms"]; d != float64(250) {
		t.Fatalf("duration_ms = %v", d)
	}
}

func TestMirrorTypeRejectsUnsupportedOpts(t *testing.T) {
	_, b, _ := testMirrorBackend(t)
	_, err := deviceDispatch(t, b, "type", map[string]any{"text": "hi", "strategy": "paste"})
	if actionCode(t, err) != "not_implemented" {
		t.Fatalf("paste: %v", err)
	}
	_, err = deviceDispatch(t, b, "type", map[string]any{"text": "hi", "require_focus": true})
	if actionCode(t, err) != "not_implemented" {
		t.Fatalf("require_focus: %v", err)
	}
}

func TestMirrorType(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	res, err := deviceDispatch(t, b, "type", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["typed_runes"] != int(5) && res.Result["typed_runes"] != 5 {
		t.Fatalf("res = %+v", res.Result)
	}
	if got := f.callList(); len(got) != 1 || got[0] != "POST /v1/type" {
		t.Fatalf("calls = %v", got)
	}
}

func TestMirrorButton(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	res, err := deviceDispatch(t, b, "button", map[string]any{"name": "home"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["backend"] != "mirror" {
		t.Fatalf("res = %+v", res.Result)
	}
	if combo := f.lastBody()["combo"]; combo != "cmd+1" {
		t.Fatalf("combo = %v", combo)
	}

	_, err = deviceDispatch(t, b, "button", map[string]any{"name": "lock"})
	if actionCode(t, err) != "not_implemented" {
		t.Fatalf("lock: %v", err)
	}
	_, err = deviceDispatch(t, b, "button", map[string]any{"name": "bogus"})
	if actionCode(t, err) != "bad_request" {
		t.Fatalf("bogus: %v", err)
	}
}

func TestMirrorPasteboardUnavailable(t *testing.T) {
	_, b, _ := testMirrorBackend(t)
	for _, kind := range []string{"pasteboard_get", "pasteboard_set"} {
		_, err := deviceDispatch(t, b, kind, map[string]any{"text": "x"})
		if actionCode(t, err) != "not_implemented" {
			t.Fatalf("%s: %v", kind, err)
		}
	}
}

func TestMirrorScreenshot(t *testing.T) {
	_, b, _ := testMirrorBackend(t)
	res, err := deviceDispatch(t, b, "screenshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["backend"] != "mirror" || res.Result["format"] != "png" {
		t.Fatalf("res keys = %+v", res.Result)
	}
	if res.Result["capture_scale"] != float64(2) {
		t.Fatalf("capture_scale = %v, want 2", res.Result["capture_scale"])
	}
	if _, err := base64.StdEncoding.DecodeString(res.Result["png_base64"].(string)); err != nil {
		t.Fatalf("bad base64: %v", err)
	}
}

func TestMirrorObserveDeclaresFidelity(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Following", "confidence": 0.95, "x": 100, "y": 40, "w": 80, "h": 20},
		{"text": "For You", "confidence": 0.97, "x": 220, "y": 40, "w": 70, "h": 20},
	}
	res, err := deviceDispatch(t, b, "observe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["backend"] != "mirror" || res.Result["fidelity"] != "ocr" {
		t.Fatalf("observe must declare reduced fidelity: %+v", res.Result)
	}
	nodes, ok := res.Result["tree"].([]*Node)
	if !ok || len(nodes) != 2 {
		t.Fatalf("tree = %#v", res.Result["tree"])
	}
	if nodes[0].Label != "Following" || nodes[0].Role != "Text" || nodes[0].Frame == nil {
		t.Fatalf("node = %+v", nodes[0])
	}
	// Interactable is a uniform compatibility constant on mirror nodes
	// (v0 additivity): OCR cannot measure it, but the field must stay.
	if !nodes[0].Interactable {
		t.Fatalf("OCR node dropped interactable: %+v", nodes[0])
	}
	if res.Result["capture_scale"] != float64(2) {
		t.Fatalf("capture_scale = %v, want 2", res.Result["capture_scale"])
	}
	if res.Result["hash"] == "" {
		t.Fatal("missing tree hash")
	}
}

func TestMirrorObserveCompact(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Profile", "confidence": 0.9, "x": 10, "y": 10, "w": 50, "h": 20},
	}
	res, err := deviceDispatch(t, b, "observe", map[string]any{"format": "compact"})
	if err != nil {
		t.Fatal(err)
	}
	txt, _ := res.Result["tree_compact"].(string)
	if !strings.Contains(txt, "Profile") {
		t.Fatalf("tree_compact = %q", txt)
	}
}

func TestMirrorObserveOCROptionsRoundTrip(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	res, err := deviceDispatch(t, b, "observe", map[string]any{
		"ocr_level": "fast",
		"rect":      map[string]any{"x": 10, "y": 20, "w": 300, "h": 400},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls := f.callList(); len(calls) != 1 || calls[0] != "POST /v1/ocr" {
		t.Fatalf("calls = %v, want [POST /v1/ocr]", calls)
	}
	body := f.lastBody()
	if body["level"] != "fast" || body["max_dim"] != float64(1300) {
		t.Fatalf("ocr body = %v", body)
	}
	rect, ok := body["rect"].(map[string]any)
	if !ok || rect["x"] != float64(10) || rect["y"] != float64(20) ||
		rect["w"] != float64(300) || rect["h"] != float64(400) {
		t.Fatalf("rect = %v", body["rect"])
	}
	if res.Result["served_scale"] != float64(0.5) || res.Result["ocr_level"] != "fast" {
		t.Fatalf("honesty metadata = %v / %v", res.Result["served_scale"], res.Result["ocr_level"])
	}
	echoed, ok := res.Result["rect"].(*mirror.OCRRect)
	if !ok || *echoed != (mirror.OCRRect{X: 10, Y: 20, W: 300, H: 400}) {
		t.Fatalf("rect echo = %#v", res.Result["rect"])
	}
}

func TestMirrorObserveDefaultAutoDownscale(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	res, err := deviceDispatch(t, b, "observe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls := f.callList(); len(calls) != 1 || calls[0] != "POST /v1/ocr" {
		t.Fatalf("calls = %v, want [POST /v1/ocr]", calls)
	}
	body := f.lastBody()
	if body["max_dim"] != float64(1300) {
		t.Fatalf("ocr body = %v, want max_dim 1300", body)
	}
	if _, ok := body["level"]; ok {
		t.Fatalf("ocr body = %v, want no level (helper default)", body)
	}
	if _, ok := body["rect"]; ok {
		t.Fatalf("ocr body = %v, want no rect", body)
	}
	if res.Result["served_scale"] != float64(0.5) || res.Result["ocr_level"] != "accurate" {
		t.Fatalf("honesty metadata = %v / %v", res.Result["served_scale"], res.Result["ocr_level"])
	}
	if _, ok := res.Result["rect"]; ok {
		t.Fatalf("rect must be absent when none was requested: %v", res.Result["rect"])
	}
}

func TestMirrorObserveLegacyHelperFallsBackToGET(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.failPost["/v1/ocr"] = mirrorFailure{status: 404, code: "bad-request",
		message: "no route POST /v1/ocr"}
	res, err := deviceDispatch(t, b, "observe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls := f.callList(); len(calls) != 2 || calls[0] != "POST /v1/ocr" || calls[1] != "GET /v1/ocr" {
		t.Fatalf("calls = %v, want [POST /v1/ocr GET /v1/ocr]", calls)
	}
	// The legacy pass ran full-resolution accurate recognition and the
	// metadata must say so.
	if res.Result["served_scale"] != float64(1) || res.Result["ocr_level"] != "accurate" {
		t.Fatalf("honesty metadata = %v / %v", res.Result["served_scale"], res.Result["ocr_level"])
	}
}

func TestMirrorObserveExplicitOptionsOnLegacyHelperFail(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.failPost["/v1/ocr"] = mirrorFailure{status: 404, code: "bad-request",
		message: "no route POST /v1/ocr"}
	_, err := deviceDispatch(t, b, "observe", map[string]any{"ocr_level": "fast"})
	if actionCode(t, err) != "bad_request" {
		t.Fatalf("explicit options must not be silently dropped: %v", err)
	}
	if calls := f.callList(); len(calls) != 1 || calls[0] != "POST /v1/ocr" {
		t.Fatalf("calls = %v, want [POST /v1/ocr] only", calls)
	}
}

func TestMirrorObserveRejectsBadOCROptions(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	for _, p := range []map[string]any{
		{"ocr_level": "fastest"},
		{"ocr_level": 3},
		{"rect": "everything"},
		{"rect": map[string]any{"x": 0, "y": 0, "w": 100}},          // missing h
		{"rect": map[string]any{"x": 0, "y": 0, "w": 100, "h": 0}},  // zero h
		{"rect": map[string]any{"x": -1, "y": 0, "w": 10, "h": 10}}, // negative x
		{"rect": map[string]any{"x": 0, "y": 0, "w": "all", "h": 10}},
	} {
		_, err := deviceDispatch(t, b, "observe", p)
		if actionCode(t, err) != "bad_request" {
			t.Fatalf("%v: %v", p, err)
		}
	}
	// Rejected before any helper call.
	if got := f.callList(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

func TestMirrorTapElement(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Sign In", "confidence": 0.96, "x": 200, "y": 600, "w": 100, "h": 30},
	}
	res, err := deviceDispatch(t, b, "tap_element", map[string]any{"label": "Sign In"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["fidelity"] != "ocr" || res.Result["backend"] != "mirror" {
		t.Fatalf("tap_element must declare OCR fidelity: %+v", res.Result)
	}
	calls := f.callList()
	if calls[len(calls)-1] != "POST /v1/tap" {
		t.Fatalf("calls = %v", calls)
	}
	body := f.lastBody()
	if body["x"] != float64(250) || body["y"] != float64(615) {
		t.Fatalf("tap at %v, want box center (250, 615)", body)
	}
}

func TestMirrorTypeIntoElementRejectsRequireFocus(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Email", "confidence": 0.9, "x": 100, "y": 300, "w": 80, "h": 24},
	}
	_, err := deviceDispatch(t, b, "type_into_element", map[string]any{
		"label": "Email", "text": "a@b.c", "require_focus": true})
	if actionCode(t, err) != "not_implemented" {
		t.Fatalf("require_focus: %v", err)
	}
	// Rejected before any UI mutation: no helper call may have happened.
	if got := f.callList(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

func TestMirrorBlockedInterstitialError(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.fail["/v1/ocr"] = mirrorFailure{status: 503, code: "blocked",
		message: "iPhone Mirroring shows a connect/paused interstitial: iphone in use"}
	_, err := deviceDispatch(t, b, "observe", nil)
	if actionCode(t, err) != "unavailable" {
		t.Fatalf("blocked: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "lock the physical phone") {
		t.Fatalf("blocked error not actionable: %s", msg)
	}
}

func TestMirrorHelperDownError(t *testing.T) {
	f, c := newFakeMirrord(t)
	f.srv.Close()
	b := NewDevice(WithDeviceRunner(newFakeRunner()),
		WithDeviceMirror(map[string]*mirror.Client{"DEV-UDID": c}),
		WithDeviceMirrorInput(map[string]bool{"DEV-UDID": true}))
	_, err := deviceDispatch(t, b, "tap", map[string]any{"x": 1, "y": 2})
	if actionCode(t, err) != "unavailable" {
		t.Fatalf("down: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "mirrord helper") {
		t.Fatalf("down error not actionable: %s", msg)
	}
}

func TestMirrorFocusLostError(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.fail["/v1/tap"] = mirrorFailure{status: 503, code: "focus-lost",
		message: "iPhone Mirroring could not be brought frontmost (Safari holds focus); input was not sent"}
	_, err := deviceDispatch(t, b, "tap", map[string]any{"x": 100, "y": 200})
	if actionCode(t, err) != "unavailable" {
		t.Fatalf("focus-lost: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "Safari holds focus") || !strings.Contains(msg, "input was not sent") {
		t.Fatalf("focus-lost error not actionable: %s", msg)
	}
}

// Stale-frontmost regression (live QA on the mini): mirrord's frontmost
// read was frozen at helper start, so /v1/status could claim
// frontmost:true forever while every focused action actually failed
// focus-lost (or worse, posted into another app). The daemon must never
// gate or soften an input result on the status snapshot — the
// per-action answer is the only authority.
func TestMirrorStaleFrontmostStatusNotTrusted(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	// The fake's status always reports frontmost:true; the tap itself
	// says focus was lost. The tap's verdict must win, verbatim.
	f.inputMode = "focused"
	f.fail["/v1/tap"] = mirrorFailure{status: 503, code: "focus-lost",
		message: "iPhone Mirroring could not be brought frontmost (Safari holds focus); input was not sent"}
	_, err := deviceDispatch(t, b, "tap", map[string]any{"x": 100, "y": 200})
	if actionCode(t, err) != "unavailable" {
		t.Fatalf("focus-lost with stale frontmost status: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "Safari holds focus") {
		t.Fatalf("error must carry the live focus holder, got: %s", msg)
	}
	// And the daemon must not have pre-checked /v1/status to decide
	// whether input may run: exactly one call, the tap itself.
	for _, c := range f.callList() {
		if c == "GET /v1/status" {
			t.Fatalf("input consulted /v1/status (stale-frontmost hazard): %v", f.callList())
		}
	}
}

// A helper that verified typing via OCR text delta (verify:"ocr-delta")
// is verified input like any other backend evidence; the verify string
// is propagated, not filtered against a known list.
func TestMirrorOCRDeltaVerifiedType(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.verify = "ocr-delta"
	res, err := deviceDispatch(t, b, "type", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["verified"] != true || res.Result["verify"] != "ocr-delta" {
		t.Fatalf("result = %+v, want verified ocr-delta", res.Result)
	}
}

func TestMirrorUntypableIsBadRequest(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.fail["/v1/type"] = mirrorFailure{status: 400, code: "untypable",
		message: "cannot type \"🎉\" via HID keycodes"}
	_, err := deviceDispatch(t, b, "type", map[string]any{"text": "🎉"})
	if actionCode(t, err) != "bad_request" {
		t.Fatalf("untypable: %v", err)
	}
}

func TestMirrorDeviceLifecycleStaysOnDevicectl(t *testing.T) {
	_, b, r := testMirrorBackend(t)
	res, err := deviceDispatch(t, b, "install_app", map[string]any{"path": "/tmp/My.app"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["installed"] != "/tmp/My.app" {
		t.Fatalf("res = %+v", res.Result)
	}
	want := "xcrun devicectl device install app --device DEV-UDID /tmp/My.app"
	if got := r.argvs(); len(got) != 1 || got[0] != want {
		t.Fatalf("calls = %v", got)
	}
}

func TestMirrorOCROptionsRoundTrip(t *testing.T) {
	f, c := newFakeMirrord(t)
	res, err := c.OCR(context.Background(), mirror.OCROptions{
		Level:  "fast",
		Rect:   &mirror.OCRRect{X: 10, Y: 20, W: 300, H: 400},
		MaxDim: 1300,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls := f.callList(); len(calls) != 1 || calls[0] != "POST /v1/ocr" {
		t.Fatalf("calls = %v, want [POST /v1/ocr]", calls)
	}
	body := f.lastBody()
	if body["level"] != "fast" || body["max_dim"] != float64(1300) {
		t.Fatalf("body = %v", body)
	}
	rect, ok := body["rect"].(map[string]any)
	if !ok || rect["x"] != float64(10) || rect["y"] != float64(20) ||
		rect["w"] != float64(300) || rect["h"] != float64(400) {
		t.Fatalf("rect = %v", body["rect"])
	}
	if res.ServedScale != 0.5 || res.Level != "fast" {
		t.Fatalf("applied metadata = scale %v level %q", res.ServedScale, res.Level)
	}
	if res.Rect == nil || *res.Rect != (mirror.OCRRect{X: 10, Y: 20, W: 300, H: 400}) {
		t.Fatalf("rect echo = %+v", res.Rect)
	}
}

func TestMirrorOCRZeroOptionsStaysLegacyGET(t *testing.T) {
	f, c := newFakeMirrord(t)
	res, err := c.OCR(context.Background(), mirror.OCROptions{})
	if err != nil {
		t.Fatal(err)
	}
	if calls := f.callList(); len(calls) != 1 || calls[0] != "GET /v1/ocr" {
		t.Fatalf("calls = %v, want [GET /v1/ocr]", calls)
	}
	if body := f.lastBody(); body != nil {
		t.Fatalf("legacy GET must carry no body, got %v", body)
	}
	if res.ServedScale != 1 || res.Level != "accurate" || res.Rect != nil {
		t.Fatalf("legacy defaults = scale %v level %q rect %+v", res.ServedScale, res.Level, res.Rect)
	}
}

// testReadOnlyMirrorBackend builds a DeviceBackend with DEV-UDID
// mirror-backed but without the allow_input opt-in (the default).
func testReadOnlyMirrorBackend(t *testing.T) (*fakeMirrord, *DeviceBackend) {
	t.Helper()
	f, c := newFakeMirrord(t)
	b := NewDevice(WithDeviceRunner(newFakeRunner()),
		WithDeviceMirror(map[string]*mirror.Client{"DEV-UDID": c}))
	return f, b
}

func TestMirrorInputGateForbidsMutatingKinds(t *testing.T) {
	f, b := testReadOnlyMirrorBackend(t)
	cases := []struct {
		kind string
		p    map[string]any
	}{
		{"tap", map[string]any{"x": 1, "y": 2}},
		{"tap", map[string]any{"x": 1, "y": 2, "duration_ms": 500}}, // long-press
		{"swipe", map[string]any{"start_x": 1, "start_y": 2, "end_x": 3, "end_y": 4}},
		{"scroll", map[string]any{"x": 1, "y": 2, "dy": 10}},
		{"type", map[string]any{"text": "hi"}},
		{"button", map[string]any{"name": "home"}},
		{"tap_element", map[string]any{"label": "OK"}},
		{"type_into_element", map[string]any{"label": "Field", "text": "hi"}},
		{"scroll_to_element", map[string]any{"label": "Row"}},
		{"scroll_until", map[string]any{"text": "Row"}},
		{"scroll_collect", map[string]any{}},
		{"tap_template", map[string]any{"name": "wifi-switch"}},
	}
	for _, c := range cases {
		_, err := deviceDispatch(t, b, c.kind, c.p)
		if actionCode(t, err) != "forbidden" {
			t.Errorf("%s: err = %v, want forbidden", c.kind, err)
		}
		if msg := err.Error(); !strings.Contains(msg, "allow_input=true") || !strings.Contains(msg, "--device-mirror-allow-input") {
			t.Errorf("%s: error not actionable: %s", c.kind, msg)
		}
	}
	// Every kind was rejected before reaching the helper.
	if got := f.callList(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

func TestMirrorInputGateAllowsReadOnlyKinds(t *testing.T) {
	f, b := testReadOnlyMirrorBackend(t)
	if res, err := deviceDispatch(t, b, "observe", nil); err != nil {
		t.Fatalf("observe: %v", err)
	} else if res.Result["backend"] != "mirror" {
		t.Fatalf("observe res = %+v", res.Result)
	}
	if res, err := deviceDispatch(t, b, "screenshot", nil); err != nil {
		t.Fatalf("screenshot: %v", err)
	} else if res.Result["backend"] != "mirror" {
		t.Fatalf("screenshot res = %+v", res.Result)
	}
	if _, err := deviceDispatch(t, b, "wait_for_element", map[string]any{
		"label": "absent", "timeout_ms": 50}); actionCode(t, err) != "timeout" {
		t.Fatalf("wait_for_element: %v, want timeout (gate must not block it)", err)
	}
	if len(f.callList()) == 0 {
		t.Fatal("read-only kinds never reached the helper")
	}
}

func TestMirrorInputGateOpensViaSetMirror(t *testing.T) {
	f, c := newFakeMirrord(t)
	b := NewDevice(WithDeviceRunner(newFakeRunner()),
		WithDeviceMirror(map[string]*mirror.Client{"DEV-UDID": c}))
	if _, err := deviceDispatch(t, b, "tap", map[string]any{"x": 1, "y": 2}); actionCode(t, err) != "forbidden" {
		t.Fatalf("gate off: %v, want forbidden", err)
	}
	// A runtime config change (POST /v0/devices, SIGHUP) can open the gate.
	b.SetMirror("DEV-UDID", f.srv.Listener.Addr().String(), true)
	if _, err := deviceDispatch(t, b, "tap", map[string]any{"x": 1, "y": 2}); err != nil {
		t.Fatalf("gate on: %v", err)
	}
	if got := f.callList(); len(got) != 1 || got[0] != "POST /v1/tap" {
		t.Fatalf("calls = %v", got)
	}
}

func TestMirrorDoesNotAffectWDADevices(t *testing.T) {
	// A backend with a mirror client for another UDID must leave this
	// device on the WDA path (which is unconfigured here).
	_, c := newFakeMirrord(t)
	b := NewDevice(WithDeviceRunner(newFakeRunner()),
		WithDeviceMirror(map[string]*mirror.Client{"OTHER-UDID": c}))
	_, err := deviceDispatch(t, b, "tap", map[string]any{"x": 1, "y": 2})
	if actionCode(t, err) != "unavailable" {
		t.Fatalf("tap: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "WebDriverAgent") {
		t.Fatalf("want WDA-unconfigured error, got: %s", msg)
	}
}

// The composite element actions carry the same per-gesture evidence as
// the direct input kinds: input_mode/verified/verify from the final
// gesture (the tap, the keystrokes) stamped on the composite's result.
func TestMirrorCompositesStampInputEvidence(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Sign In", "confidence": 0.96, "x": 200, "y": 600, "w": 100, "h": 30},
		{"text": "Email", "confidence": 0.9, "x": 100, "y": 300, "w": 80, "h": 24},
	}
	for _, tc := range []struct {
		kind    string
		payload map[string]any
	}{
		{"tap_element", map[string]any{"label": "Sign In"}},
		{"type_into_element", map[string]any{"label": "Email", "text": "a@b.c"}},
	} {
		res, err := deviceDispatch(t, b, tc.kind, tc.payload)
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		if res.Result["input_mode"] != "background" {
			t.Fatalf("%s result = %+v, want input_mode background", tc.kind, res.Result)
		}
		if res.Result["verified"] != true || res.Result["verify"] != "pixel-delta" {
			t.Fatalf("%s result = %+v, want verified pixel-delta", tc.kind, res.Result)
		}
		if res.Result["fidelity"] != "ocr" || res.Result["backend"] != "mirror" {
			t.Fatalf("%s result = %+v, want mirror/ocr fidelity", tc.kind, res.Result)
		}
	}
}

// tap_template's mirror tap carries the same per-gesture evidence as
// every other mirror input: input_mode/verified/verify from the ack
// stamped on the result.
func TestMirrorTapTemplateStampsInputEvidence(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	frame := texturedFrame(64, 64)
	var buf bytes.Buffer
	if err := png.Encode(&buf, frame); err != nil {
		t.Fatal(err)
	}
	f.png, f.pngW, f.pngH = buf.Bytes(), 64, 64
	tmpl := cropPNGBytes(t, frame, image.Rect(16, 24, 40, 40))
	res, err := deviceDispatch(t, b, "tap_template", map[string]any{
		"template_base64": base64.StdEncoding.EncodeToString(tmpl),
	})
	if err != nil {
		t.Fatalf("tap_template: %v", err)
	}
	if res.Result["input_mode"] != "background" {
		t.Fatalf("result = %+v, want input_mode background", res.Result)
	}
	if res.Result["verified"] != true || res.Result["verify"] != "pixel-delta" {
		t.Fatalf("result = %+v, want verified pixel-delta", res.Result)
	}
	if res.Result["backend"] != "mirror" || res.Result["matched"] != true {
		t.Fatalf("result = %+v, want matched mirror result", res.Result)
	}
}

// An unverified gesture inside a composite fails the whole action, same
// as the direct kinds: no silent posted-but-unproven tap_element.
func TestMirrorCompositeUnverifiedGestureFails(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.verified = false
	f.verify = "pixel-delta"
	f.boxes = []map[string]any{
		{"text": "Sign In", "confidence": 0.96, "x": 200, "y": 600, "w": 100, "h": 30},
	}
	_, err := deviceDispatch(t, b, "tap_element", map[string]any{"label": "Sign In"})
	if got := actionCode(t, err); got != "unavailable" {
		t.Fatalf("code = %q (%v), want unavailable", got, err)
	}
	if !strings.Contains(err.Error(), "did not change") {
		t.Fatalf("error = %v, want the screen-unchanged verdict", err)
	}
}

// A scroll_to_element whose target is already on screen posts no gesture
// and must not stamp input fields (there is no ack to report) — and an
// ack from an earlier action must not leak into it.
func TestMirrorScrollToVisibleElementStampsNoInputFields(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Row", "confidence": 0.9, "x": 100, "y": 300, "w": 80, "h": 24},
	}
	// An earlier action's ack (a direct tap) must not leak forward.
	if _, err := deviceDispatch(t, b, "tap", map[string]any{"x": 1, "y": 2}); err != nil {
		t.Fatal(err)
	}
	res, err := deviceDispatch(t, b, "scroll_to_element", map[string]any{"label": "Row"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Result["input_mode"]; ok {
		t.Fatalf("res = %+v, a gesture-free composite must not claim an input mode", res.Result)
	}
	if _, ok := res.Result["verified"]; ok {
		t.Fatalf("res = %+v, a gesture-free composite must not claim verification", res.Result)
	}
}

// Concurrent composites on one device each report their own gesture's
// evidence: the per-call context holder cannot cross-talk.
func TestMirrorConcurrentCompositesKeepOwnEvidence(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Row", "confidence": 0.9, "x": 100, "y": 300, "w": 80, "h": 24},
	}
	const n = 4
	results := make([]map[string]any, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := deviceDispatch(t, b, "tap_element", map[string]any{"label": "Row"})
			if err == nil {
				results[i] = res.Result
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("dispatch %d: %v", i, errs[i])
		}
		if results[i]["input_mode"] != "background" || results[i]["verified"] != true {
			t.Fatalf("dispatch %d result = %+v, want its own gesture evidence", i, results[i])
		}
	}
}
