package mcp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

// toolText extracts the first text-content string from a tools/call response.
func toolText(t *testing.T, resp map[string]any) (text string, isError bool) {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in response: %v", resp)
	}
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("no content in result: %v", res)
	}
	first, _ := content[0].(map[string]any)
	text, _ = first["text"].(string)
	isError, _ = res["isError"].(bool)
	return text, isError
}

// lastAction returns the most recent action the fake daemon received.
func lastAction(t *testing.T, f *fakeDaemon) proto.ActionRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.actions) == 0 {
		t.Fatal("no actions dispatched")
	}
	return f.actions[len(f.actions)-1]
}

func TestElementToolsHappyPath(t *testing.T) {
	cases := []struct {
		tool     string
		args     string
		wantKind string
		// payload fields that must be forwarded verbatim
		wantPayload map[string]any
		// result returned by the fake daemon
		result map[string]any
		// substrings the tool's JSON text must contain
		wantText []string
	}{
		{
			tool: "tap_element", args: `{"lease_id":"lse_mcp","label":"Continue","anchor":"end"}`,
			wantKind:    "tap_element",
			wantPayload: map[string]any{"label": "Continue", "anchor": "end", "ax_hashes": true},
			result: map[string]any{"x": 100.0, "y": 200.0,
				"ax_before": "aaa", "ax_after": "bbb"},
			wantText: []string{`"ui_changed":true`, `"x":100`},
		},
		{
			tool: "type_into_element", args: `{"lease_id":"lse_mcp","placeholder":"Email","text":"a@b.c","strategy":"paste"}`,
			wantKind: "type_into_element",
			wantPayload: map[string]any{"placeholder": "Email", "text": "a@b.c",
				"strategy": "paste", "ax_hashes": true},
			result: map[string]any{"typed_runes": 5.0,
				"ax_before": "aaa", "ax_after": "aaa"},
			wantText: []string{`"ui_changed":false`, `"typed_runes":5`},
		},
		{
			tool: "scroll_to_element", args: `{"lease_id":"lse_mcp","id":"row-42","direction":"up","max_scrolls":3}`,
			wantKind: "scroll_to_element",
			wantPayload: map[string]any{"id": "row-42", "direction": "up",
				"max_scrolls": 3.0, "ax_hashes": true},
			result: map[string]any{"scrolls": 2.0, "hash": "h1",
				"ax_before": "aaa", "ax_after": "bbb"},
			wantText: []string{`"scrolls":2`, `"hash":"h1"`, `"ui_changed":true`},
		},
		{
			tool: "wait_for_element", args: `{"lease_id":"lse_mcp","label":"Done","absent":true,"timeout_ms":5000}`,
			wantKind:    "wait_for_element",
			wantPayload: map[string]any{"label": "Done", "absent": true, "timeout_ms": 5000.0},
			result:      map[string]any{"absent": true, "polls": 3.0, "hash": "h4"},
			wantText:    []string{`"absent":true`, `"hash":"h4"`},
		},
		{
			tool: "wait_tree_stable", args: `{"lease_id":"lse_mcp","stable_samples":4}`,
			wantKind:    "wait_tree_stable",
			wantPayload: map[string]any{"stable_samples": 4.0},
			result:      map[string]any{"stable": true, "hash": "h2"},
			wantText:    []string{`"stable":true`, `"hash":"h2"`},
		},
		{
			tool: "ui_tree", args: `{"lease_id":"lse_mcp"}`,
			wantKind:    "observe",
			wantPayload: map[string]any{},
			result:      map[string]any{"tree": []any{}, "hash": "h3"},
			wantText:    []string{`"hash":"h3"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			f := newFakeDaemon()
			defer f.Close()
			f.onAction = func(req proto.ActionRequest) (int, any) {
				return http.StatusOK, proto.ActionResult{OK: true, Result: tc.result}
			}
			resps := runSession(t, f, call(1, tc.tool, tc.args))
			text, isError := toolText(t, resps[0])
			if isError {
				t.Fatalf("unexpected tool error: %s", text)
			}
			for _, want := range tc.wantText {
				if !strings.Contains(text, want) {
					t.Fatalf("result text missing %q: %s", want, text)
				}
			}
			a := lastAction(t, f)
			if a.Kind != tc.wantKind || a.LeaseID != "lse_mcp" {
				t.Fatalf("dispatched %q lease %q, want kind %q", a.Kind, a.LeaseID, tc.wantKind)
			}
			for k, v := range tc.wantPayload {
				got, err := json.Marshal(a.Payload[k])
				if err != nil {
					t.Fatal(err)
				}
				want, _ := json.Marshal(v)
				if string(got) != string(want) {
					t.Fatalf("payload[%q] = %s, want %s (payload %v)", k, got, want, a.Payload)
				}
			}
		})
	}
}

func TestElementToolsMatcherMissHasHint(t *testing.T) {
	cases := []struct {
		tool string
		args string
		code string
		// substring the error text must contain (the next-step hint)
		wantHint string
	}{
		{"tap_element", `{"lease_id":"lse_mcp","label":"Missing"}`, "timeout", "call ui_tree"},
		{"tap_element", `{"lease_id":"lse_mcp","label":"Below"}`, "off_viewport", "scroll_to_element"},
		{"type_into_element", `{"lease_id":"lse_mcp","label":"Field","text":"x"}`, "timeout", "call ui_tree"},
		{"scroll_to_element", `{"lease_id":"lse_mcp","label":"Missing"}`, "timeout", "call ui_tree"},
		{"wait_for_element", `{"lease_id":"lse_mcp","label":"Missing"}`, "timeout", "call ui_tree"},
		{"wait_for_element", `{"lease_id":"lse_mcp","label":"Spinner","absent":true}`, "timeout", "still on screen"},
	}
	for _, tc := range cases {
		t.Run(tc.tool+"_"+tc.code, func(t *testing.T) {
			f := newFakeDaemon()
			defer f.Close()
			f.onAction = func(req proto.ActionRequest) (int, any) {
				return http.StatusRequestTimeout, proto.Error{
					Code: tc.code, Message: `element (label="Missing") did not appear within the 10s budget`}
			}
			resps := runSession(t, f, call(1, tc.tool, tc.args))
			text, isError := toolText(t, resps[0])
			if !isError {
				t.Fatalf("expected isError result, got: %s", text)
			}
			if !strings.Contains(text, tc.wantHint) {
				t.Fatalf("error text missing hint %q: %s", tc.wantHint, text)
			}
		})
	}
}

func TestWaitTreeStableReturnsInnerResult(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	f.onAction = func(req proto.ActionRequest) (int, any) {
		return http.StatusOK, proto.ActionResult{OK: true, Result: map[string]any{
			"stable": true, "hash": "h2", "settled_ms": 1200.0, "samples": 3.0}}
	}
	resps := runSession(t, f, call(1, "wait_tree_stable", `{"lease_id":"lse_mcp"}`))
	text, isError := toolText(t, resps[0])
	if isError {
		t.Fatalf("unexpected tool error: %s", text)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("result is not a JSON object: %v (%s)", err, text)
	}
	// stable/hash must be TOP-LEVEL, not wrapped in an ok/result envelope.
	if got["stable"] != true || got["hash"] != "h2" {
		t.Fatalf("stable/hash not top-level: %s", text)
	}
	for _, k := range []string{"ok", "result", "journal_ref"} {
		if _, wrapped := got[k]; wrapped {
			t.Fatalf("result still carries the %q envelope field: %s", k, text)
		}
	}
}

func TestMissingHashesReportUIChangedNull(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	// Daemon under load: post-action hash timed out, only ax_before came back.
	f.onAction = func(req proto.ActionRequest) (int, any) {
		return http.StatusOK, proto.ActionResult{OK: true, Result: map[string]any{
			"x": 100.0, "y": 200.0, "ax_before": "aaa"}}
	}
	resps := runSession(t, f, call(1, "tap_element", `{"lease_id":"lse_mcp","label":"Continue"}`))
	text, isError := toolText(t, resps[0])
	if isError {
		t.Fatalf("unexpected tool error: %s", text)
	}
	if !strings.Contains(text, `"ui_changed":null`) {
		t.Fatalf("ui_changed should be explicit null when a hash is missing: %s", text)
	}
}

func TestNilResultStillReportsUIChangedNull(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	// Daemon replied OK with no result data at all (nil map client-side).
	f.onAction = func(req proto.ActionRequest) (int, any) {
		return http.StatusOK, proto.ActionResult{OK: true}
	}
	resps := runSession(t, f, call(1, "tap_element", `{"lease_id":"lse_mcp","label":"Continue"}`))
	text, isError := toolText(t, resps[0])
	if isError {
		t.Fatalf("unexpected tool error: %s", text)
	}
	if !strings.Contains(text, `"ui_changed":null`) {
		t.Fatalf("ui_changed should be explicit null on an empty result: %s", text)
	}
}

func TestScrollWithoutSwipeOmitsUIChanged(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	f.onAction = func(req proto.ActionRequest) (int, any) {
		return http.StatusOK, proto.ActionResult{OK: true, Result: map[string]any{
			"scrolls": 0.0, "hash": "h1", "ax_before": "aaa", "ax_after": "bbb"}}
	}
	resps := runSession(t, f, call(1, "scroll_to_element", `{"lease_id":"lse_mcp","label":"Visible"}`))
	text, isError := toolText(t, resps[0])
	if isError {
		t.Fatalf("unexpected tool error: %s", text)
	}
	if strings.Contains(text, "ui_changed") {
		t.Fatalf("ui_changed should be omitted when no swipe happened: %s", text)
	}
}

func TestElementToolsDaemonErrorSurfaces(t *testing.T) {
	for _, tool := range []string{"tap_element", "type_into_element", "scroll_to_element",
		"wait_for_element", "wait_tree_stable", "ui_tree"} {
		t.Run(tool, func(t *testing.T) {
			f := newFakeDaemon()
			defer f.Close()
			f.onAction = func(req proto.ActionRequest) (int, any) {
				return http.StatusServiceUnavailable, proto.Error{
					Code: "unavailable", Message: "a11y bridge not ready"}
			}
			args := `{"lease_id":"lse_mcp","label":"X","text":"y"}`
			resps := runSession(t, f, call(1, tool, args))
			text, isError := toolText(t, resps[0])
			if !isError || !strings.Contains(text, "a11y bridge not ready") {
				t.Fatalf("expected daemon error to surface, got isError=%v text=%s", isError, text)
			}
		})
	}
}

func TestElementToolsRequireLeaseAndText(t *testing.T) {
	cases := []struct {
		tool string
		args string
		want string
	}{
		{"tap_element", `{"label":"X"}`, "lease_id"},
		{"type_into_element", `{"lease_id":"lse_mcp","label":"X"}`, "text"},
		{"scroll_to_element", `{"label":"X"}`, "lease_id"},
		{"wait_for_element", `{"label":"X"}`, "lease_id"},
		{"wait_tree_stable", `{}`, "lease_id"},
		{"ui_tree", `{}`, "lease_id"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			f := newFakeDaemon()
			defer f.Close()
			resps := runSession(t, f, call(1, tc.tool, tc.args))
			text, isError := toolText(t, resps[0])
			if !isError || !strings.Contains(text, tc.want) {
				t.Fatalf("expected %q argument error, got isError=%v text=%s", tc.want, isError, text)
			}
		})
	}
}
