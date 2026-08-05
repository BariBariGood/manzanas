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
)

// fakeWDA is an httptest server mimicking WebDriverAgent's wire protocol.
type fakeWDA struct {
	mu         sync.Mutex
	sessions   int
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
		f.mu.Lock()
		f.sessions++
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
	mux.HandleFunc("GET /source", func(w http.ResponseWriter, r *http.Request) {
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

func TestScreenshot(t *testing.T) {
	_, c := newFake(t)
	png, err := c.Screenshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(png) != "fake-png-bytes" {
		t.Fatalf("png = %q", png)
	}
}

func TestSource(t *testing.T) {
	_, c := newFake(t)
	xml, err := c.Source(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(xml, "XCUIElementTypeApplication") {
		t.Fatalf("xml = %q", xml)
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
