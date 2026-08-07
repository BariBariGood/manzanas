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
	// onAction, when set, overrides the canned POST /v0/actions response:
	// it returns the HTTP status and the body to encode.
	onAction  func(req proto.ActionRequest) (int, any)
	actionErr *proto.Error
	actionSt  int
	// targetState is UDID-1's reported state (default Booted); a boot
	// request flips it to Booted and is recorded in bootCalls.
	targetState proto.TargetState
	targetKind  proto.TargetKind
	bootErr     *proto.Error
	bootStalls  bool
	bootCalls   []string
	leaseReqs   []proto.AcquireLeaseRequest
}

func (f *fakeDaemon) setTargetState(s proto.TargetState) {
	f.mu.Lock()
	f.targetState = s
	f.mu.Unlock()
}

// failActions makes every subsequent POST /v0/actions fail with the given
// protocol error.
func (f *fakeDaemon) failActions(status int, code, msg string) {
	f.mu.Lock()
	f.actionErr = &proto.Error{Code: code, Message: msg}
	f.actionSt = status
	f.mu.Unlock()
}

func newFakeDaemon() *fakeDaemon {
	f := &fakeDaemon{}
	mux := http.NewServeMux()
	f.targetState = proto.StateBooted
	f.targetKind = proto.TargetSimulator
	mux.HandleFunc("GET /v0/targets", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		state, kind := f.targetState, f.targetKind
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"targets": []proto.Target{
			{UDID: "UDID-1", Name: "iPhone 17 Pro", State: state, Kind: kind,
				Labels: []string{"simulator", "ios26"}},
		}})
	})
	mux.HandleFunc("POST /v0/targets/{udid}/boot", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.bootCalls = append(f.bootCalls, r.PathValue("udid")+"?wait="+r.URL.Query().Get("wait"))
		bootErr := f.bootErr
		if bootErr == nil && !f.bootStalls {
			f.targetState = proto.StateBooted
		}
		f.mu.Unlock()
		if bootErr != nil {
			writeJSON(w, http.StatusNotFound, bootErr)
			return
		}
		writeJSON(w, http.StatusAccepted, proto.Target{UDID: r.PathValue("udid"), State: proto.StateBooted})
	})
	mux.HandleFunc("POST /v0/leases", func(w http.ResponseWriter, r *http.Request) {
		var req proto.AcquireLeaseRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.leaseReqs = append(f.leaseReqs, req)
		f.mu.Unlock()
		exp := time.Now().Add(5 * time.Minute)
		writeJSON(w, http.StatusCreated, proto.Lease{ID: "lse_mcp", State: proto.LeaseActive,
			TargetUDID: "UDID-1", TTLSeconds: 300, ExpiresAt: &exp})
	})
	mux.HandleFunc("POST /v0/leases/{id}/renew", func(w http.ResponseWriter, r *http.Request) {
		exp := time.Now().Add(10 * time.Minute)
		writeJSON(w, http.StatusOK, proto.Lease{ID: r.PathValue("id"), State: proto.LeaseActive,
			TargetUDID: "UDID-1", TTLSeconds: 600, ExpiresAt: &exp})
	})
	mux.HandleFunc("DELETE /v0/leases/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.released = append(f.released, r.PathValue("id"))
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, proto.Lease{ID: r.PathValue("id"), State: proto.LeaseReleased})
	})
	mux.HandleFunc("GET /v0/journal/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"run_id": r.PathValue("run_id"),
			"meta": map[string]any{"format_version": "journal/v0",
				"run_id": r.PathValue("run_id"), "agent_id": "agent-mcp"},
			"entries": []map[string]any{{
				"ref":  map[string]any{"run_id": r.PathValue("run_id"), "seq": 1},
				"kind": "action",
				"payload": map[string]any{"action": "targets.boot", "status": "error",
					"error": "sim wedged"},
			}},
			"next_seq": 0,
		})
	})
	mux.HandleFunc("POST /v0/actions", func(w http.ResponseWriter, r *http.Request) {
		var req proto.ActionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.actions = append(f.actions, req)
		onAction := f.onAction
		failErr, failSt := f.actionErr, f.actionSt
		f.mu.Unlock()
		if onAction != nil {
			code, body := onAction(req)
			writeJSON(w, code, body)
			return
		}
		if failErr != nil {
			writeJSON(w, failSt, failErr)
			return
		}
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
	s := New(client.New(f.URL), "test")
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
	instr, _ := init["instructions"].(string)
	if !strings.Contains(instr, "lease_acquire") {
		t.Fatalf("initialize missing usage instructions: %v", init)
	}
	info := init["serverInfo"].(map[string]any)
	if info["name"] != "manzanas" || info["version"] != "test" {
		t.Fatalf("bad serverInfo: %v", info)
	}
	tools := resps[1]["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"lease_acquire", "lease_release", "lease_renew", "targets",
		"observe", "tap", "swipe", "type_text", "button", "screenshot", "app", "state",
		"ui_tree", "tap_element", "type_into_element", "scroll_to_element",
		"wait_for_element", "wait_tree_stable"} {
		if !names[want] {
			t.Fatalf("tool %q missing from tools/list: %v", want, names)
		}
	}
	for _, tl := range tools {
		m := tl.(map[string]any)
		props, _ := m["inputSchema"].(map[string]any)["properties"].(map[string]any)
		for pname, p := range props {
			desc, _ := p.(map[string]any)["description"].(string)
			if desc == "" {
				t.Errorf("tool %s property %s has no description", m["name"], pname)
			}
		}
	}
}

func TestLeaseAcquireBootsShutdownTarget(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	f.setTargetState(proto.StateShutdown)
	resps := runSession(t, f, call(1, "lease_acquire", `{"labels":["ios26"]}`))
	res := resps[0]["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("acquire failed: %v", res)
	}
	f.mu.Lock()
	boots := append([]string(nil), f.bootCalls...)
	f.mu.Unlock()
	if len(boots) != 1 || boots[0] != "UDID-1?wait=true" {
		t.Fatalf("expected one boot?wait=true call, got %v", boots)
	}
}

func TestLeaseAcquireBootRespectsOptOutAndBootedTarget(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	// Already-booted target: no boot call.
	runSession(t, f, call(1, "lease_acquire", `{}`))
	// Shutdown target with boot=false: no boot call either.
	f.setTargetState(proto.StateShutdown)
	runSession(t, f, call(1, "lease_acquire", `{"boot":false}`))
	// Physical device (even with a non-Booted state): never booted; the
	// lease is returned as-is.
	f.mu.Lock()
	f.targetKind = proto.TargetDevice
	f.targetState = proto.StateUnknown
	f.mu.Unlock()
	resps := runSession(t, f, call(1, "lease_acquire", `{}`))
	if res := resps[0]["result"].(map[string]any); res["isError"] == true {
		t.Fatalf("device lease_acquire failed: %v", res)
	}
	f.mu.Lock()
	boots := append([]string(nil), f.bootCalls...)
	f.mu.Unlock()
	if len(boots) != 0 {
		t.Fatalf("expected no boot calls, got %v", boots)
	}
}

// TestLeaseAcquireToleratesEndpointWithoutBoot simulates a manzanas-broker
// fleet endpoint: leases and targets are served, but there is no
// /v0/targets/{udid}/boot route (the mux answers a plain 404). The granted
// lease must be returned, not turned into a boot failure.
func TestLeaseAcquireToleratesEndpointWithoutBoot(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/targets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"targets": []proto.Target{
			{UDID: "UDID-1", Kind: proto.TargetSimulator, State: proto.StateShutdown,
				Labels: []string{"simulator", "ios26"}},
		}})
	})
	mux.HandleFunc("POST /v0/leases", func(w http.ResponseWriter, r *http.Request) {
		exp := time.Now().Add(5 * time.Minute)
		writeJSON(w, http.StatusCreated, proto.Lease{ID: "lse_broker", State: proto.LeaseActive,
			TargetUDID: "UDID-1", TTLSeconds: 300, ExpiresAt: &exp})
	})
	mux.HandleFunc("DELETE /v0/leases/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, proto.Lease{ID: r.PathValue("id"), State: proto.LeaseReleased})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	in := strings.NewReader(call(1, "lease_acquire", `{"labels":["ios26"]}`) + "\n")
	var out bytes.Buffer
	if err := New(client.New(srv.URL), "test").Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var m map[string]any
	if err := json.NewDecoder(&out).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := m["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("acquire against boot-less endpoint failed: %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "lse_broker") {
		t.Fatalf("lease not returned: %s", text)
	}
}

// A JSON 404 with code not_found from a real daemon (lease or target
// vanished) must surface as a boot_error on the returned lease — never as
// an isError that hides the held lease, and never silently.
func TestLeaseAcquireBootNotFoundWarns(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	f.setTargetState(proto.StateShutdown)
	f.mu.Lock()
	f.bootErr = &proto.Error{Code: proto.ErrNotFound, Message: "lease not found"}
	f.mu.Unlock()
	assertLeaseWithBootError(t, runSession(t, f, call(1, "lease_acquire", `{}`)))
}

// The boot wait budget expiring must not fail the acquire: the lease is
// usable, so it is returned with a boot_error warning.
func TestLeaseAcquireBootBudgetExpiryWarns(t *testing.T) {
	oldPoll, oldBudget := bootWaitPoll, bootWaitBudget
	bootWaitPoll, bootWaitBudget = time.Millisecond, 10*time.Millisecond
	defer func() { bootWaitPoll, bootWaitBudget = oldPoll, oldBudget }()
	f := newFakeDaemon()
	defer f.Close()
	f.setTargetState(proto.StateShutdown)
	f.mu.Lock()
	f.bootStalls = true // boot accepted but target never reaches Booted
	f.mu.Unlock()
	assertLeaseWithBootError(t, runSession(t, f, call(1, "lease_acquire", `{}`)))
}

func assertLeaseWithBootError(t *testing.T, resps []map[string]any) {
	t.Helper()
	res := resps[0]["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("expected granted lease with boot_error, got isError: %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "lse_mcp") || !strings.Contains(text, "boot_error") {
		t.Fatalf("expected lease JSON with boot_error, got: %s", text)
	}
}

func TestLeaseAcquireForwardsIdentity(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	runSession(t, f, call(1, "lease_acquire",
		`{"agent_id":"claude-code-7","purpose":"login QA","reset":"erase"}`))
	runSession(t, f, call(1, "lease_acquire", `{}`))
	f.mu.Lock()
	reqs := append([]proto.AcquireLeaseRequest(nil), f.leaseReqs...)
	f.mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("expected 2 acquire requests, got %d", len(reqs))
	}
	if reqs[0].AgentID != "claude-code-7" || reqs[0].Purpose != "login QA" || reqs[0].Reset != "erase" {
		t.Fatalf("identity not forwarded: %+v", reqs[0])
	}
	if reqs[1].AgentID != "manzanas-mcp" {
		t.Fatalf("default agent_id missing: %+v", reqs[1])
	}
}

func TestLeaseRenew(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	resps := runSession(t, f, call(1, "lease_renew", `{"lease_id":"lse_mcp","ttl_seconds":600}`))
	res := resps[0]["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("renew failed: %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "lse_mcp") || !strings.Contains(text, "600") {
		t.Fatalf("unexpected renew result: %s", text)
	}
}

func TestErrorsCarryRecoveryHints(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	f.failActions(http.StatusGone, proto.ErrLeaseExpired, "lease lse_mcp expired")
	resps := runSession(t, f, call(1, "tap", `{"lease_id":"lse_mcp","x":1,"y":2}`))
	res := resps[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("expected isError, got %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "lease_acquire") {
		t.Fatalf("expired-lease error missing recovery hint: %q", text)
	}

	// Daemon unreachable: the hint tells the agent how to fix the connection.
	dead := New(client.New("http://127.0.0.1:1"), "test")
	in := strings.NewReader(call(1, "targets", `{}`) + "\n")
	var out bytes.Buffer
	if err := dead.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var m map[string]any
	if err := json.NewDecoder(&out).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	text = m["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "MANZANASD_ADDR") {
		t.Fatalf("conn error missing recovery hint: %q", text)
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

func TestJournalExportTool(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	resps := runSession(t, f,
		call(1, "journal_export", `{"run_id":"lse_mcp"}`),
		call(2, "journal_export", `{"run_id":"lse_mcp","format":"json"}`),
		call(3, "journal_export", `{}`),
		call(4, "journal_export", `{"run_id":"lse_mcp","format":"xml"}`),
	)
	byID := map[float64]map[string]any{}
	for _, r := range resps {
		byID[r["id"].(float64)] = r["result"].(map[string]any)
	}
	text := func(res map[string]any) string {
		return res["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	md := text(byID[1])
	for _, want := range []string{
		"## manzanasd run journal — `lse_mcp`",
		"| Result | **FAILED** (1 of 1 steps errored) |",
		"| **error** | sim wedged |",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown export missing %q:\n%s", want, md)
		}
	}
	var doc struct {
		RunID    string `json:"run_id"`
		Failures int    `json:"failures"`
	}
	if err := json.Unmarshal([]byte(text(byID[2])), &doc); err != nil {
		t.Fatalf("json export not JSON: %v", err)
	}
	if doc.RunID != "lse_mcp" || doc.Failures != 1 {
		t.Fatalf("json export wrong: %+v", doc)
	}
	if byID[3]["isError"] != true || byID[4]["isError"] != true {
		t.Fatalf("bad args must be isError results: %v / %v", byID[3], byID[4])
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
