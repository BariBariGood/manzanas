package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/client"
	"github.com/BariBariGood/manzanas/proto"
)

// fakeDaemon records action dispatches and serves minimal lease endpoints.
type fakeDaemon struct {
	*httptest.Server
	mu       sync.Mutex
	actions  []proto.ActionRequest
	released []string
}

func newFakeDaemon() *fakeDaemon {
	f := &fakeDaemon{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/targets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"targets": []proto.Target{
			{UDID: "UDID-1", Name: "iPhone 17 Pro", State: proto.StateBooted,
				Labels: []string{"simulator", "ios26"}},
		}})
	})
	mux.HandleFunc("POST /v0/leases", func(w http.ResponseWriter, r *http.Request) {
		exp := time.Now().Add(5 * time.Minute)
		writeJSON(w, http.StatusCreated, proto.Lease{ID: "lse_mcp", State: proto.LeaseActive,
			TargetUDID: "UDID-1", TTLSeconds: 300, ExpiresAt: &exp})
	})
	mux.HandleFunc("DELETE /v0/leases/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.released = append(f.released, r.PathValue("id"))
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, proto.Lease{ID: r.PathValue("id"), State: proto.LeaseReleased})
	})
	mux.HandleFunc("POST /v0/actions", func(w http.ResponseWriter, r *http.Request) {
		var req proto.ActionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.actions = append(f.actions, req)
		f.mu.Unlock()
		res := map[string]any{}
		if req.Kind == "screenshot" {
			res["data_base64"] = "aGVsbG8="
		}
		writeJSON(w, http.StatusOK, proto.ActionResult{OK: true, Result: res})
	})
	f.Server = httptest.NewServer(mux)
	return f
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// runSession feeds newline-delimited requests through a Server and returns
// the decoded responses in order.
func runSession(t *testing.T, f *fakeDaemon, requests ...string) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")
	var out bytes.Buffer
	s := New(client.New(f.URL))
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var responses []map[string]any
	dec := json.NewDecoder(&out)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		responses = append(responses, m)
	}
	return responses
}

func call(id int, name string, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, args)
}

func TestInitializeAndListTools(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	resps := runSession(t, f,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(resps) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(resps))
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" {
		t.Fatalf("bad initialize result: %v", init)
	}
	tools := resps[1]["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"lease_acquire", "lease_release", "targets", "observe",
		"tap", "swipe", "type_text", "button", "screenshot", "app", "state"} {
		if !names[want] {
			t.Fatalf("tool %q missing from tools/list: %v", want, names)
		}
	}
}

func TestToolCallDispatchesAction(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	resps := runSession(t, f,
		call(1, "tap", `{"lease_id":"lse_mcp","x":50,"y":60}`),
		call(2, "type_text", `{"lease_id":"lse_mcp","text":"hi"}`),
	)
	if len(resps) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(resps))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(f.actions))
	}
	// Tool calls run concurrently, so daemon-side order is not guaranteed.
	byKind := map[string]proto.ActionRequest{}
	for _, a := range f.actions {
		byKind[a.Kind] = a
	}
	tap, ok := byKind["tap"]
	if !ok || tap.Payload["x"] != float64(50) || tap.LeaseID != "lse_mcp" {
		t.Fatalf("tap not mapped: %+v", f.actions)
	}
	typ, ok := byKind["type"]
	if !ok || typ.Payload["text"] != "hi" {
		t.Fatalf("type not mapped: %+v", f.actions)
	}
}

func TestScreenshotReturnsImageContent(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	resps := runSession(t, f, call(1, "screenshot", `{"lease_id":"lse_mcp"}`))
	content := resps[0]["result"].(map[string]any)["content"].([]any)
	img := content[0].(map[string]any)
	if img["type"] != "image" || img["mimeType"] != "image/png" || img["data"] != "aGVsbG8=" {
		t.Fatalf("bad image content: %v", img)
	}
}

func TestToolErrorIsResultWithIsError(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	resps := runSession(t, f, call(1, "tap", `{"x":1,"y":2}`)) // missing lease_id
	res := resps[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("expected isError result, got %v", resps[0])
	}
}

func TestUnknownMethodAndTool(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	resps := runSession(t, f,
		`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`,
		call(2, "nope", `{}`),
	)
	if resps[0]["error"] == nil || resps[1]["error"] == nil {
		t.Fatalf("expected errors: %v", resps)
	}
}

func TestSessionReleasesOwnedLeasesOnExit(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	runSession(t, f, call(1, "lease_acquire", `{"labels":["ios26"]}`))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.released) != 1 || f.released[0] != "lse_mcp" {
		t.Fatalf("lease not auto-released: %v", f.released)
	}
}
