package wda

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeWDA is an httptest server mimicking WebDriverAgent's wire protocol.
type fakeWDA struct {
	mu         sync.Mutex
	sessions   int
	caps       []map[string]any
	taps       [][2]float64
	keys       []string
	swipes     [][5]float64
	buttons    []string
	locks      int
	pasteboard string
	// dropSession makes the next session-scoped call fail with
	// "invalid session id" once, exercising the re-create path.
	dropSession bool
}

func (f *fakeWDA) handler() http.Handler {
	mux := http.NewServeMux()
	writeValue := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"value": v})
	}
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeValue(w, map[string]any{"ready": true})
	})
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Capabilities struct {
				AlwaysMatch map[string]any `json:"alwaysMatch"`
			} `json:"capabilities"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.sessions++
		f.caps = append(f.caps, body.Capabilities.AlwaysMatch)
		sid := fmt.Sprintf("sess-%d", f.sessions)
		f.mu.Unlock()
		writeValue(w, map[string]any{"sessionId": sid, "capabilities": map[string]any{}})
	})
	sessionGate := func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		drop := f.dropSession
		f.dropSession = false
		sid := fmt.Sprintf("sess-%d", f.sessions)
		f.mu.Unlock()
		if drop || !strings.Contains(r.URL.Path, "/session/"+sid+"/") {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"value": map[string]any{
				"error": "invalid session id", "message": "session does not exist"}})
			return false
		}
		return true
	}
	mux.HandleFunc("POST /session/{sid}/wda/tap", func(w http.ResponseWriter, r *http.Request) {
		if !sessionGate(w, r) {
			return
		}
		var body struct{ X, Y float64 }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.taps = append(f.taps, [2]float64{body.X, body.Y})
		f.mu.Unlock()
		writeValue(w, nil)
	})
	mux.HandleFunc("POST /session/{sid}/wda/keys", func(w http.ResponseWriter, r *http.Request) {
		if !sessionGate(w, r) {
			return
		}
		var body struct{ Value []string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.keys = append(f.keys, strings.Join(body.Value, ""))
		f.mu.Unlock()
		writeValue(w, nil)
	})
	mux.HandleFunc("POST /session/{sid}/wda/dragfromtoforduration", func(w http.ResponseWriter, r *http.Request) {
		if !sessionGate(w, r) {
			return
		}
		var body struct{ FromX, FromY, ToX, ToY, Duration float64 }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.swipes = append(f.swipes, [5]float64{body.FromX, body.FromY, body.ToX, body.ToY, body.Duration})
		f.mu.Unlock()
		writeValue(w, nil)
	})
	mux.HandleFunc("POST /session/{sid}/wda/pressButton", func(w http.ResponseWriter, r *http.Request) {
		if !sessionGate(w, r) {
			return
		}
		var body struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.buttons = append(f.buttons, body.Name)
		f.mu.Unlock()
		writeValue(w, nil)
	})
	mux.HandleFunc("POST /session/{sid}/wda/lock", func(w http.ResponseWriter, r *http.Request) {
		if !sessionGate(w, r) {
			return
		}
		f.mu.Lock()
		f.locks++
		f.mu.Unlock()
		writeValue(w, nil)
	})
	mux.HandleFunc("POST /session/{sid}/wda/setPasteboard", func(w http.ResponseWriter, r *http.Request) {
		if !sessionGate(w, r) {
			return
		}
		var body struct{ Content string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		decoded, _ := base64.StdEncoding.DecodeString(body.Content)
		f.mu.Lock()
		f.pasteboard = string(decoded)
		f.mu.Unlock()
		writeValue(w, nil)
	})
	mux.HandleFunc("POST /session/{sid}/wda/getPasteboard", func(w http.ResponseWriter, r *http.Request) {
		if !sessionGate(w, r) {
			return
		}
		f.mu.Lock()
		text := f.pasteboard
		f.mu.Unlock()
		writeValue(w, base64.StdEncoding.EncodeToString([]byte(text)))
	})
	mux.HandleFunc("GET /screenshot", func(w http.ResponseWriter, r *http.Request) {
		writeValue(w, base64.StdEncoding.EncodeToString([]byte("fake-png-bytes")))
	})
	mux.HandleFunc("GET /session/{sid}/source", func(w http.ResponseWriter, r *http.Request) {
		if !sessionGate(w, r) {
			return
		}
		writeValue(w, `<XCUIElementTypeApplication name="Test"/>`)
	})
	return mux
}

func newFake(t *testing.T) (*fakeWDA, *Client) {
	t.Helper()
	f := &fakeWDA{}
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	return f, New(ts.URL)
}

func TestStatus(t *testing.T) {
	_, c := newFake(t)
	if err := c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTapCreatesSessionLazily(t *testing.T) {
	f, c := newFake(t)
	if err := c.Tap(context.Background(), 100, 200.5); err != nil {
		t.Fatal(err)
	}
	if f.sessions != 1 {
		t.Fatalf("sessions = %d, want 1", f.sessions)
	}
	if len(f.taps) != 1 || f.taps[0] != [2]float64{100, 200.5} {
		t.Fatalf("taps = %v", f.taps)
	}
	// A second tap reuses the session.
	if err := c.Tap(context.Background(), 1, 2); err != nil {
		t.Fatal(err)
	}
	if f.sessions != 1 {
		t.Fatalf("sessions = %d after second tap, want 1", f.sessions)
	}
}

func TestInvalidSessionRetriesOnce(t *testing.T) {
	f, c := newFake(t)
	if err := c.Tap(context.Background(), 1, 1); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.dropSession = true
	f.mu.Unlock()
	if err := c.Tap(context.Background(), 3, 4); err != nil {
		t.Fatalf("tap after dropped session should retry: %v", err)
	}
	if f.sessions != 2 {
		t.Fatalf("sessions = %d, want 2 (re-created)", f.sessions)
	}
}

func TestKeys(t *testing.T) {
	f, c := newFake(t)
	if err := c.Keys(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if len(f.keys) != 1 || f.keys[0] != "hello" {
		t.Fatalf("keys = %v", f.keys)
	}
}

// Screenshot must handle both response shapes WDA builds answer with:
// the JSON envelope {"value":"<base64>"} (upstream) and raw image bytes
// (real-world builds).
func TestScreenshotShapes(t *testing.T) {
	rawPNG := append(append([]byte{}, pngMagic...), []byte("pixel-data")...)
	cases := []struct {
		name        string
		contentType string
		body        []byte
		want        []byte
	}{
		{
			name:        "json envelope base64",
			contentType: "application/json",
			body: []byte(fmt.Sprintf(`{"value":%q}`,
				base64.StdEncoding.EncodeToString([]byte("fake-png-bytes")))),
			want: []byte("fake-png-bytes"),
		},
		{
			name:        "raw png bytes",
			contentType: "image/png",
			body:        rawPNG,
			want:        rawPNG,
		},
		{
			// PNG magic alone must be enough: some builds answer raw
			// bytes with a generic content type.
			name:        "raw png bytes, no image content type",
			contentType: "application/octet-stream",
			body:        rawPNG,
			want:        rawPNG,
		},
		{
			name:        "image content type, non-png bytes",
			contentType: "image/jpeg",
			body:        []byte("\xff\xd8\xffjpeg-data"),
			want:        []byte("\xff\xd8\xffjpeg-data"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = w.Write(tc.body)
			}))
			t.Cleanup(ts.Close)
			got, err := New(ts.URL).Screenshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A failed screenshot must still surface as a typed *Error.
func TestScreenshotError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"value": map[string]any{
			"error": "unknown error", "message": "screenshot boom"}})
	}))
	t.Cleanup(ts.Close)
	_, err := New(ts.URL).Screenshot(context.Background())
	var we *Error
	if !errors.As(err, &we) || we.Status != 500 || we.Message != "screenshot boom" {
		t.Fatalf("got %T %v", err, err)
	}
}

func TestSourceUsesSessionRoute(t *testing.T) {
	f, c := newFake(t)
	xml, err := c.Source(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(xml, "XCUIElementTypeApplication") {
		t.Fatalf("xml = %q", xml)
	}
	if f.sessions != 1 {
		t.Fatalf("sessions = %d, want 1 (source must be session-scoped)", f.sessions)
	}
}

// wantCaps are the quiescence-off capabilities every session create must
// carry (issue #155: empty caps crash the runner on large apps).
func wantCaps(t *testing.T, got map[string]any) {
	t.Helper()
	want := map[string]any{
		"waitForQuiescence":         false,
		"shouldWaitForQuiescence":   false,
		"waitForIdleTimeout":        float64(0),
		"shouldUseCompactResponses": true,
		"useFirstMatch":             true,
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("caps[%q] = %v, want %v (all caps: %v)", k, got[k], v, got)
		}
	}
}

func TestSessionCreatedWithQuiescenceOff(t *testing.T) {
	f, c := newFake(t)
	if err := c.Tap(context.Background(), 1, 1); err != nil {
		t.Fatal(err)
	}
	if len(f.caps) != 1 {
		t.Fatalf("caps = %v, want one session create", f.caps)
	}
	wantCaps(t, f.caps[0])
}

// The caps must be re-applied when the session is re-created after the
// runner restarts (the invalid-session retry path).
func TestSessionCapsReappliedOnRecreate(t *testing.T) {
	f, c := newFake(t)
	if err := c.Tap(context.Background(), 1, 1); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.dropSession = true
	f.mu.Unlock()
	if err := c.Tap(context.Background(), 2, 2); err != nil {
		t.Fatal(err)
	}
	if len(f.caps) != 2 {
		t.Fatalf("caps = %v, want two session creates", f.caps)
	}
	wantCaps(t, f.caps[1])
}

// A client-side round-trip timeout means the runner is slow, not dead:
// it must not be reported as a restart, and the next action must not
// pay a readiness wait or lose the session.
func TestClientTimeoutIsNotARestart(t *testing.T) {
	f := &fakeWDA{}
	var mu sync.Mutex
	slow := false
	inner := f.handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		s := slow
		mu.Unlock()
		if s && strings.HasSuffix(r.URL.Path, "/wda/tap") {
			time.Sleep(300 * time.Millisecond)
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	c := New(ts.URL)
	c.httpc.Timeout = 100 * time.Millisecond
	if err := c.Tap(context.Background(), 1, 1); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	slow = true
	mu.Unlock()
	err := c.Tap(context.Background(), 2, 2)
	if err == nil || strings.Contains(err.Error(), "WDA runner restarted") {
		t.Fatalf("client timeout must not be reported as a restart: %v", err)
	}
	c.mu.Lock()
	needsReady, sid := c.needsReady, c.sessionID
	c.mu.Unlock()
	if needsReady || sid == "" {
		t.Fatalf("client timeout must not arm readiness wait or drop the session (needsReady=%v sessionID=%q)", needsReady, sid)
	}
}

// A transport-level failure (the runner died mid-action) must surface a
// retryable message rather than a bare connection error, and the next
// action must first wait for /status readiness.
func TestRunnerDeathSurfacesRetryableAndWaitsForReadiness(t *testing.T) {
	f := &fakeWDA{}
	var mu sync.Mutex
	down := false
	statusProbes := 0
	inner := f.handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.URL.Path == "/status" {
			statusProbes++
		}
		d := down
		mu.Unlock()
		if d {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server does not support hijacking")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			conn.Close() // abrupt close: connection reset mid-action
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	c := New(ts.URL)
	c.readyWait = 2 * time.Second
	if err := c.Tap(context.Background(), 1, 1); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	down = true
	mu.Unlock()
	err := c.Tap(context.Background(), 2, 2)
	if err == nil || !strings.Contains(err.Error(), "WDA runner restarted, retry") {
		t.Fatalf("want retryable runner-restart error, got: %v", err)
	}
	mu.Lock()
	down = false
	statusProbes = 0
	mu.Unlock()
	// The next action must probe /status before proceeding, and the
	// dropped session must be re-created with the caps re-applied.
	if err := c.Tap(context.Background(), 3, 3); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	probes := statusProbes
	mu.Unlock()
	if probes == 0 {
		t.Fatal("expected a /status readiness probe before the first action after runner death")
	}
	if f.sessions != 2 {
		t.Fatalf("sessions = %d, want 2 (re-created after restart)", f.sessions)
	}
	wantCaps(t, f.caps[1])
}

// While the runner stays down, only the first action after the observed
// death pays the readiness wait; follow-up actions fail fast instead of
// re-arming the wait, and a recovered runner restores the normal cycle.
func TestExhaustedReadinessWaitFailsFast(t *testing.T) {
	f := &fakeWDA{}
	var mu sync.Mutex
	down := false
	inner := f.handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		d := down
		mu.Unlock()
		if d {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server does not support hijacking")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	c := New(ts.URL)
	c.readyWait = time.Second
	if err := c.Tap(context.Background(), 1, 1); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	down = true
	mu.Unlock()
	if err := c.Tap(context.Background(), 2, 2); err == nil {
		t.Fatal("want error while runner is down")
	}
	// Second action pays the readiness wait once (runner still down).
	if err := c.Tap(context.Background(), 2, 2); err == nil {
		t.Fatal("want error while runner is down")
	}
	// Subsequent actions must fail fast, not re-pay the wait.
	start := time.Now()
	if err := c.Tap(context.Background(), 2, 2); err == nil {
		t.Fatal("want error while runner is down")
	}
	if el := time.Since(start); el > c.readyWait/2 {
		t.Fatalf("action after exhausted readiness wait took %v, want fast fail", el)
	}
	// A recovered runner resets the cycle: actions succeed again.
	mu.Lock()
	down = false
	mu.Unlock()
	if err := c.Tap(context.Background(), 3, 3); err != nil {
		t.Fatal(err)
	}
}

func TestSwipe(t *testing.T) {
	f, c := newFake(t)
	if err := c.Swipe(context.Background(), 10, 20, 30, 40, 0.5); err != nil {
		t.Fatal(err)
	}
	if len(f.swipes) != 1 || f.swipes[0] != [5]float64{10, 20, 30, 40, 0.5} {
		t.Fatalf("swipes = %v", f.swipes)
	}
}

func TestPressButtonAndLock(t *testing.T) {
	f, c := newFake(t)
	if err := c.PressButton(context.Background(), "volumeUp"); err != nil {
		t.Fatal(err)
	}
	if err := c.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.buttons) != 1 || f.buttons[0] != "volumeUp" || f.locks != 1 {
		t.Fatalf("buttons = %v locks = %d", f.buttons, f.locks)
	}
}

func TestPasteboardRoundTrip(t *testing.T) {
	f, c := newFake(t)
	if err := c.SetPasteboard(context.Background(), "hello clipboard"); err != nil {
		t.Fatal(err)
	}
	if f.pasteboard != "hello clipboard" {
		t.Fatalf("pasteboard = %q", f.pasteboard)
	}
	got, err := c.GetPasteboard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello clipboard" {
		t.Fatalf("got %q", got)
	}
}

func TestErrorShape(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"value": map[string]any{
			"error": "unknown error", "message": "boom"}})
	}))
	t.Cleanup(ts.Close)
	c := New(ts.URL)
	err := c.Status(context.Background())
	var we *Error
	if !errors.As(err, &we) || we.Status != 500 || we.Message != "boom" {
		t.Fatalf("got %T %v", err, err)
	}
}

// A non-JSON error body (a proxy error page, a restarted runner) must
// still surface as a typed *Error carrying the HTTP status and raw body.
func TestNonJSONErrorBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>invalid session id</html>"))
	}))
	t.Cleanup(ts.Close)
	c := New(ts.URL)
	err := c.Status(context.Background())
	var we *Error
	if !errors.As(err, &we) || we.Status != 404 || !strings.Contains(we.Message, "invalid session id") {
		t.Fatalf("got %T %v", err, err)
	}
	if !isInvalidSession(err) {
		t.Fatalf("expected invalid-session classification: %v", err)
	}
}
