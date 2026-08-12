package actions

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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
}

type mirrorFailure struct {
	status  int
	code    string
	message string
}

func newFakeMirrord(t *testing.T) (*fakeMirrord, *mirror.Client) {
	t.Helper()
	f := &fakeMirrord{t: t, fail: map[string]mirrorFailure{}}
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
	boxes := f.boxes
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if failing {
		w.WriteHeader(fail.status)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": fail.code, "error": fail.message})
		return
	}
	switch r.URL.Path {
	case "/v1/status":
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "ready", "frontmost": true})
	case "/v1/screenshot":
		pngBytes := tinyPNGBytes(f.t)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"png_base64": base64.StdEncoding.EncodeToString(pngBytes),
			"img_w":      4, "img_h": 4,
		})
	case "/v1/ocr":
		if boxes == nil {
			boxes = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"boxes": boxes, "img_w": 600, "img_h": 1300})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
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
	b := NewDevice(WithDeviceRunner(r), WithDeviceMirror(map[string]*mirror.Client{"DEV-UDID": c}))
	return f, b, r
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
		WithDeviceMirror(map[string]*mirror.Client{"DEV-UDID": c}))
	_, err := deviceDispatch(t, b, "tap", map[string]any{"x": 1, "y": 2})
	if actionCode(t, err) != "unavailable" {
		t.Fatalf("down: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "mirrord helper") {
		t.Fatalf("down error not actionable: %s", msg)
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
