package actions

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeWDAServer is a minimal WebDriverAgent stand-in for device backend
// tests: enough of the wire protocol for sessions, HID, pasteboard,
// screenshot, and source.
type fakeWDAServer struct {
	mu         sync.Mutex
	taps       [][2]float64
	keys       []string
	swipes     [][5]float64
	buttons    []string
	locks      int
	pasteboard string
	sourceXML  string
	down       bool
}

func (f *fakeWDAServer) start(t *testing.T) string {
	t.Helper()
	writeValue := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"value": v})
	}
	gate := func(w http.ResponseWriter) bool {
		f.mu.Lock()
		down := f.down
		f.mu.Unlock()
		if down {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"value": map[string]any{
				"error": "unknown error", "message": "socket hang up"}})
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		if gate(w) {
			writeValue(w, map[string]any{"ready": true})
		}
	})
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		if gate(w) {
			writeValue(w, map[string]any{"sessionId": "s1"})
		}
	})
	mux.HandleFunc("POST /session/s1/wda/tap", func(w http.ResponseWriter, r *http.Request) {
		if !gate(w) {
			return
		}
		var b struct{ X, Y float64 }
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.taps = append(f.taps, [2]float64{b.X, b.Y})
		f.mu.Unlock()
		writeValue(w, nil)
	})
	mux.HandleFunc("POST /session/s1/wda/keys", func(w http.ResponseWriter, r *http.Request) {
		if !gate(w) {
			return
		}
		var b struct{ Value []string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.keys = append(f.keys, strings.Join(b.Value, ""))
		f.mu.Unlock()
		writeValue(w, nil)
	})
	mux.HandleFunc("POST /session/s1/wda/dragfromtoforduration", func(w http.ResponseWriter, r *http.Request) {
		if !gate(w) {
			return
		}
		var b struct{ FromX, FromY, ToX, ToY, Duration float64 }
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.swipes = append(f.swipes, [5]float64{b.FromX, b.FromY, b.ToX, b.ToY, b.Duration})
		f.mu.Unlock()
		writeValue(w, nil)
	})
	mux.HandleFunc("POST /session/s1/wda/pressButton", func(w http.ResponseWriter, r *http.Request) {
		if !gate(w) {
			return
		}
		var b struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.buttons = append(f.buttons, b.Name)
		f.mu.Unlock()
		writeValue(w, nil)
	})
	mux.HandleFunc("POST /session/s1/wda/lock", func(w http.ResponseWriter, r *http.Request) {
		if !gate(w) {
			return
		}
		f.mu.Lock()
		f.locks++
		f.mu.Unlock()
		writeValue(w, nil)
	})
	mux.HandleFunc("POST /session/s1/wda/setPasteboard", func(w http.ResponseWriter, r *http.Request) {
		if !gate(w) {
			return
		}
		var b struct{ Content string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		decoded, _ := base64.StdEncoding.DecodeString(b.Content)
		f.mu.Lock()
		f.pasteboard = string(decoded)
		f.mu.Unlock()
		writeValue(w, nil)
	})
	mux.HandleFunc("POST /session/s1/wda/getPasteboard", func(w http.ResponseWriter, r *http.Request) {
		if !gate(w) {
			return
		}
		f.mu.Lock()
		text := f.pasteboard
		f.mu.Unlock()
		writeValue(w, base64.StdEncoding.EncodeToString([]byte(text)))
	})
	mux.HandleFunc("GET /screenshot", func(w http.ResponseWriter, r *http.Request) {
		if gate(w) {
			writeValue(w, base64.StdEncoding.EncodeToString([]byte("png")))
		}
	})
	mux.HandleFunc("GET /session/s1/source", func(w http.ResponseWriter, r *http.Request) {
		if !gate(w) {
			return
		}
		f.mu.Lock()
		src := f.sourceXML
		f.mu.Unlock()
		writeValue(w, src)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

func testDeviceBackendWithWDA(t *testing.T, f *fakeWDAServer, opts ...DeviceOption) *DeviceBackend {
	t.Helper()
	url := f.start(t)
	opts = append([]DeviceOption{
		WithDeviceRunner(newFakeRunner()),
		WithDeviceWDA(map[string]string{"DEV-UDID": url}),
	}, opts...)
	return NewDevice(opts...)
}

func TestDeviceSwipe(t *testing.T) {
	f := &fakeWDAServer{sourceXML: sampleWDAXML}
	b := testDeviceBackendWithWDA(t, f)
	res, err := deviceDispatch(t, b, "swipe", map[string]any{
		"start_x": 100, "start_y": 600, "end_x": 100, "end_y": 200, "duration_seconds": 0.3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["backend"] != "wda" {
		t.Fatalf("res = %+v", res)
	}
	if len(f.swipes) != 1 || f.swipes[0] != [5]float64{100, 600, 100, 200, 0.3} {
		t.Fatalf("swipes = %v", f.swipes)
	}

	_, err = deviceDispatch(t, b, "swipe", map[string]any{"start_x": 1})
	if actionCode(t, err) != "bad_request" {
		t.Fatalf("missing coords: %v", err)
	}
}

func TestDeviceButton(t *testing.T) {
	f := &fakeWDAServer{sourceXML: sampleWDAXML}
	b := testDeviceBackendWithWDA(t, f)
	for name, want := range map[string]string{"home": "home", "volume-up": "volumeUp", "volume-down": "volumeDown"} {
		if _, err := deviceDispatch(t, b, "button", map[string]any{"name": name}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, got := range f.buttons {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: buttons = %v, want %s", name, f.buttons, want)
		}
	}
	if _, err := deviceDispatch(t, b, "button", map[string]any{"name": "lock"}); err != nil {
		t.Fatal(err)
	}
	if f.locks != 1 {
		t.Fatalf("locks = %d", f.locks)
	}
	_, err := deviceDispatch(t, b, "button", map[string]any{"name": "siri"})
	if actionCode(t, err) != "bad_request" {
		t.Fatalf("unsupported button: %v", err)
	}
}

func TestDevicePasteboard(t *testing.T) {
	f := &fakeWDAServer{sourceXML: sampleWDAXML}
	b := testDeviceBackendWithWDA(t, f)
	if _, err := deviceDispatch(t, b, "pasteboard_set", map[string]any{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	res, err := deviceDispatch(t, b, "pasteboard_get", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["text"] != "hello" {
		t.Fatalf("res = %+v", res)
	}
}

func TestDeviceObserveReturnsTree(t *testing.T) {
	f := &fakeWDAServer{sourceXML: sampleWDAXML}
	b := testDeviceBackendWithWDA(t, f)
	res, err := deviceDispatch(t, b, "observe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["hash"] == "" || res.Result["tree"] == nil {
		t.Fatalf("res = %+v", res)
	}
	if _, ok := res.Result["raw"]; ok {
		t.Fatal("raw XML should be omitted by default")
	}
	res, err = deviceDispatch(t, b, "observe", map[string]any{"include_raw": true})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := res.Result["raw"].(string)
	if !strings.Contains(raw, "XCUIElementTypeButton") {
		t.Fatalf("raw = %q", raw)
	}
}

func TestDeviceTapElement(t *testing.T) {
	f := &fakeWDAServer{sourceXML: sampleWDAXML}
	b := testDeviceBackendWithWDA(t, f)
	res, err := deviceDispatch(t, b, "tap_element", map[string]any{"label": "Log In"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.taps) != 1 {
		t.Fatalf("taps = %v", f.taps)
	}
	// Log In frame is x=20 y=700 w=353 h=44 -> centre 196.5, 722.
	if f.taps[0] != [2]float64{196.5, 722} {
		t.Fatalf("tap = %v, want centre of Log In", f.taps[0])
	}
	if res.Result["element"] == nil {
		t.Fatalf("res = %+v", res)
	}
}

func TestDeviceTypeIntoElement(t *testing.T) {
	f := &fakeWDAServer{sourceXML: sampleWDAXML}
	b := testDeviceBackendWithWDA(t, f)
	res, err := deviceDispatch(t, b, "type_into_element", map[string]any{"label": "Email", "text": "x@y.z"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.taps) != 1 || len(f.keys) != 1 || f.keys[0] != "x@y.z" {
		t.Fatalf("taps = %v keys = %v", f.taps, f.keys)
	}
	if res.Result["typed_runes"] != 5 {
		t.Fatalf("res = %+v", res)
	}
}

func TestDeviceWaitForElement(t *testing.T) {
	f := &fakeWDAServer{sourceXML: sampleWDAXML}
	b := testDeviceBackendWithWDA(t, f)
	res, err := deviceDispatch(t, b, "wait_for_element",
		map[string]any{"label": "Welcome", "timeout_ms": 500})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["element"] == nil {
		t.Fatalf("res = %+v", res)
	}

	_, err = deviceDispatch(t, b, "wait_for_element",
		map[string]any{"label": "No Such Thing", "timeout_ms": 150, "interval_ms": 50})
	if actionCode(t, err) != "timeout" {
		t.Fatalf("got %v, want timeout", err)
	}
}

func TestDeviceWaitTreeStable(t *testing.T) {
	f := &fakeWDAServer{sourceXML: sampleWDAXML}
	b := testDeviceBackendWithWDA(t, f)
	res, err := deviceDispatch(t, b, "wait_tree_stable",
		map[string]any{"timeout_ms": 2000, "interval_ms": 20})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["stable"] != true {
		t.Fatalf("res = %+v", res)
	}
}

func TestDeviceWDAFailureKicksSupervisor(t *testing.T) {
	f := &fakeWDAServer{sourceXML: sampleWDAXML, down: true}
	kicked := 0
	b := testDeviceBackendWithWDA(t, f,
		WithDeviceWDAKick(map[string]func(){"DEV-UDID": func() { kicked++ }}))
	_, err := deviceDispatch(t, b, "tap", map[string]any{"x": 1, "y": 2})
	if actionCode(t, err) != "unavailable" {
		t.Fatalf("got %v, want unavailable", err)
	}
	if kicked == 0 {
		t.Fatal("supervisor kick was not invoked on WDA failure")
	}
}
