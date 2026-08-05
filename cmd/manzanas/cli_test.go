package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/client"
)

// runCLI runs one manzanas command against the fake daemon and returns
// stdout, stderr, and the error.
func runCLI(t *testing.T, f *fakeDaemon, jsonOut bool, args ...string) (string, string, error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	app := &appEnv{client: client.New(f.URL), json: jsonOut, stdout: &out, stderr: &errBuf}
	cmd, ok := commands[args[0]]
	if !ok {
		t.Fatalf("unknown command %q", args[0])
	}
	err := cmd(context.Background(), app, args[1:])
	return out.String(), errBuf.String(), err
}

func TestTargetsMapsToProtocol(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	out, _, err := runCLI(t, f, false, "targets")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got.Method != "GET" || got.Path != "/v0/targets" {
		t.Fatalf("wrong request: %+v", got)
	}
	if !strings.Contains(out, "UDID-1") || !strings.Contains(out, "iphone-17-pro") {
		t.Fatalf("human output missing target: %q", out)
	}
	out, _, err = runCLI(t, f, true, "targets")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("--json output not JSON: %v", err)
	}
}

func TestLeaseAcquireMapsBody(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	out, _, err := runCLI(t, f, false, "lease", "acquire",
		"--labels", "ios26,iphone-17-pro", "--agent", "tester", "--ttl", "600",
		"--udid", "UDID-1", "--reset", "erase")
	if err != nil {
		t.Fatal(err)
	}
	req := f.last()
	if req.Method != "POST" || req.Path != "/v0/leases" {
		t.Fatalf("wrong request: %+v", req)
	}
	labels, _ := req.Body["labels"].([]any)
	if len(labels) != 2 || labels[0] != "ios26" || labels[1] != "iphone-17-pro" {
		t.Fatalf("labels not mapped: %v", req.Body)
	}
	if req.Body["agent_id"] != "tester" || req.Body["ttl_seconds"] != float64(600) {
		t.Fatalf("body not mapped: %v", req.Body)
	}
	if req.Body["udid"] != "UDID-1" || req.Body["reset"] != "erase" {
		t.Fatalf("udid/reset not mapped: %v", req.Body)
	}
	if !strings.Contains(out, "lse_test") || !strings.Contains(out, "active") {
		t.Fatalf("output: %q", out)
	}
}

func TestLeaseAcquireWaitSurfacesQueuePosition(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	out, stderr, err := runCLI(t, f, false, "lease", "acquire",
		"--agent", "tester", "--purpose", "queue-me", "--wait")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "queued at position 2") {
		t.Fatalf("queue position not surfaced: %q", stderr)
	}
	if !strings.Contains(out, "active") {
		t.Fatalf("did not end active: %q", out)
	}
}

func TestLeaseRenewReleaseLs(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	if _, _, err := runCLI(t, f, false, "lease", "renew", "lse_test", "--ttl", "600"); err != nil {
		t.Fatal(err)
	}
	req := f.last()
	if req.Path != "/v0/leases/lse_test/renew" || req.Body["ttl_seconds"] != float64(600) {
		t.Fatalf("renew not mapped: %+v", req)
	}
	if _, _, err := runCLI(t, f, false, "lease", "release", "lse_test"); err != nil {
		t.Fatal(err)
	}
	if req = f.last(); req.Method != "DELETE" || req.Path != "/v0/leases/lse_test" {
		t.Fatalf("release not mapped: %+v", req)
	}
	out, _, err := runCLI(t, f, false, "lease", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if req = f.last(); req.Method != "GET" || req.Path != "/v0/leases" {
		t.Fatalf("ls not mapped: %+v", req)
	}
	if !strings.Contains(out, "UDID-1") {
		t.Fatalf("ls output: %q", out)
	}
}

func TestActionCommandsMapToDispatch(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	cases := []struct {
		args    []string
		kind    string
		payload map[string]any
	}{
		{[]string{"tap", "100", "200", "--lease", "lse_test"}, "tap",
			map[string]any{"x": float64(100), "y": float64(200)}},
		{[]string{"swipe", "10", "20", "30", "40", "--duration-ms", "250", "--lease", "lse_test"}, "swipe",
			map[string]any{"start_x": float64(10), "start_y": float64(20), "end_x": float64(30), "end_y": float64(40), "duration_seconds": float64(0.25)}},
		{[]string{"type", "hello world", "--lease", "lse_test"}, "type",
			map[string]any{"text": "hello world"}},
		{[]string{"button", "home", "--lease", "lse_test"}, "button",
			map[string]any{"name": "home"}},
		{[]string{"observe", "--lease", "lse_test"}, "observe",
			map[string]any{}},
		{[]string{"app", "install", "/tmp/My.app", "--lease", "lse_test"}, "install_app",
			map[string]any{"path": "/tmp/My.app"}},
		{[]string{"app", "launch", "com.example.app", "--lease", "lse_test"}, "launch_app",
			map[string]any{"bundle_id": "com.example.app"}},
		{[]string{"app", "terminate", "com.example.app", "--lease", "lse_test"}, "terminate_app",
			map[string]any{"bundle_id": "com.example.app"}},
	}
	for _, tc := range cases {
		if _, _, err := runCLI(t, f, false, tc.args...); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		req := f.last()
		if req.Path != "/v0/actions" || req.Body["kind"] != tc.kind || req.Body["lease_id"] != "lse_test" {
			t.Fatalf("%v: wrong request %+v", tc.args, req)
		}
		payload, _ := req.Body["payload"].(map[string]any)
		for k, want := range tc.payload {
			if payload[k] != want {
				t.Fatalf("%v: payload[%s]=%v want %v", tc.args, k, payload[k], want)
			}
		}
	}
}

func TestScreenshotWritesFile(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	out := filepath.Join(t.TempDir(), "shot.png")
	if _, _, err := runCLI(t, f, false, "screenshot", "-o", out, "--lease", "lse_test"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || string(data[1:4]) != "PNG" {
		t.Fatalf("not a PNG: %d bytes", len(data))
	}
}

func TestBootShutdownAndStreamAndState(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	if _, _, err := runCLI(t, f, false, "boot", "UDID-1", "--lease", "lse_test"); err != nil {
		t.Fatal(err)
	}
	req := f.last()
	if req.Path != "/v0/targets/UDID-1/boot" || req.Body["lease_id"] != "lse_test" {
		t.Fatalf("boot not mapped: %+v", req)
	}
	if _, _, err := runCLI(t, f, false, "shutdown", "UDID-1", "--lease", "lse_test"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, f, false, "stream", "url", "--lease", "lse_test", "--format", "mjpeg")
	if err != nil {
		t.Fatal(err)
	}
	if want := f.URL + "/view/UDID-1"; !strings.Contains(out, want) {
		t.Fatalf("stream url output: %q, want %q", out, want)
	}
	out, _, err = runCLI(t, f, true, "stream", "url", "--lease", "lse_test")
	if err != nil {
		t.Fatal(err)
	}
	wsWant := "ws://" + strings.TrimPrefix(f.URL, "http://") + "/v0/streams/stm_1/ws"
	if !strings.Contains(out, wsWant) || !strings.Contains(out, f.URL+"/v0/streams/stm_1/mjpeg") {
		t.Fatalf("stream url --json output: %q", out)
	}
	out, _, err = runCLI(t, f, false, "state", "snapshot", "--lease", "lse_test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "snap_1") {
		t.Fatalf("snapshot output: %q", out)
	}
	if _, _, err := runCLI(t, f, false, "state", "restore", "snap_1", "--lease", "lse_test"); err != nil {
		t.Fatal(err)
	}
	req = f.last()
	if req.Path != "/v0/state/restore" || req.Body["snapshot"] != "snap_1" {
		t.Fatalf("restore not mapped: %+v", req)
	}
	if _, _, err := runCLI(t, f, false, "state", "fixture", "statusbar",
		"--payload", `{"time":"9:41"}`, "--lease", "lse_test"); err != nil {
		t.Fatal(err)
	}
	req = f.last()
	payload, _ := req.Body["payload"].(map[string]any)
	if req.Body["name"] != "statusbar" || payload["time"] != "9:41" {
		t.Fatalf("fixture not mapped: %+v", req)
	}
}

func TestJournalTail(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	out, _, err := runCLI(t, f, false, "journal", "tail", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	req := f.last()
	if req.Method != "GET" || req.Path != "/v0/journal/run_1" {
		t.Fatalf("journal not mapped: %+v", req)
	}
	if !strings.Contains(out, "action") {
		t.Fatalf("journal output: %q", out)
	}
	// The fake daemon clamps pages to 2 entries and paginates via next_seq:
	// a --limit above the clamp must still drain all 3 entries.
	out, _, err = runCLI(t, f, false, "journal", "tail", "run_1", "--limit", "2000")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 action", "2 action", "3 action"} {
		if !strings.Contains(out, want) {
			t.Fatalf("journal pagination dropped %q: %q", want, out)
		}
	}
}

func TestDaemonUnreachableIsFriendly(t *testing.T) {
	var out, errBuf bytes.Buffer
	app := &appEnv{client: client.New("http://127.0.0.1:1"), stdout: &out, stderr: &errBuf}
	err := cmdTargets(context.Background(), app, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot reach manzanasd") {
		t.Fatalf("expected friendly conn error, got %v", err)
	}
}

func TestNotImplementedSurfacesCode(t *testing.T) {
	f := newFakeDaemon()
	f.actionsNotImplemented = true
	defer f.Close()
	_, _, err := runCLI(t, f, false, "tap", "1", "2", "--lease", "lse_test")
	if err == nil || !strings.Contains(err.Error(), "not_implemented") {
		t.Fatalf("expected not_implemented error, got %v", err)
	}
}
