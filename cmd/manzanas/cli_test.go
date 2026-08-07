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
	"github.com/BariBariGood/manzanas/internal/journal"
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
	stdout, stderr, err := runCLI(t, f, false, "screenshot", "-o", out, "--lease", "lse_test")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || string(data[1:4]) != "PNG" {
		t.Fatalf("not a PNG: %d bytes", len(data))
	}
	// Status goes to stderr so a piped stdout never carries stray text
	// alongside binary/machine-readable output.
	if stdout != "" {
		t.Fatalf("stdout must stay empty with -o FILE, got %q", stdout)
	}
	if !strings.Contains(stderr, "wrote") {
		t.Fatalf("status line missing from stderr: %q", stderr)
	}
}

// -o - writes the raw image bytes to stdout, with status only on stderr:
// `manzanas screenshot -o - > shot.png` must produce a valid PNG.
func TestScreenshotToStdoutIsPureBinary(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	stdout, stderr, err := runCLI(t, f, false, "screenshot", "-o", "-", "--lease", "lse_test")
	if err != nil {
		t.Fatal(err)
	}
	if len(stdout) == 0 || stdout[1:4] != "PNG" {
		t.Fatalf("stdout is not a bare PNG (first bytes %q)", stdout[:min(8, len(stdout))])
	}
	if !strings.Contains(stderr, "wrote") {
		t.Fatalf("status line missing from stderr: %q", stderr)
	}
	// --json promises pure JSON on stdout, which -o - cannot honor: the
	// combination is rejected during flag validation, before any action
	// is dispatched (nothing for the daemon to capture or journal).
	before := len(f.requests)
	stdout, _, err = runCLI(t, f, true, "screenshot", "-o", "-", "--lease", "lse_test")
	if err == nil || !strings.Contains(err.Error(), "--json") {
		t.Fatalf("--json with -o - must be rejected, got err=%v stdout=%q", err, stdout)
	}
	if stdout != "" {
		t.Fatalf("rejected --json -o - must not write to stdout: %q", stdout)
	}
	if got := len(f.requests); got != before {
		t.Fatalf("rejection must happen before dispatch: daemon saw %d new request(s)", got-before)
	}
}

// --json with -o FILE keeps the machine-readable summary on stdout (no
// binary there to corrupt) and nothing else mixed in.
func TestScreenshotJSONSummaryOnStdout(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	out := filepath.Join(t.TempDir(), "shot.png")
	stdout, _, err := runCLI(t, f, true, "screenshot", "-o", out, "--lease", "lse_test")
	if err != nil {
		t.Fatal(err)
	}
	var summary struct {
		File  string `json:"file"`
		Bytes int    `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("--json stdout not JSON: %v (%q)", err, stdout)
	}
	if summary.File != out || summary.Bytes == 0 {
		t.Fatalf("summary = %+v", summary)
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

func TestJournalUpload(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	dir := t.TempDir()
	shot1 := filepath.Join(dir, "104-rm-today.png")
	shot2 := filepath.Join(dir, "105-rm-today2.png")
	if err := os.WriteFile(shot1, []byte("png-1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shot2, []byte("png-2"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, f, false, "journal", "upload", "run_1", shot1, shot2)
	if err != nil {
		t.Fatal(err)
	}
	req := f.last()
	if req.Method != "POST" ||
		req.Path != "/v0/journal/run_1/artifacts?kind=screenshot&name=105-rm-today2.png" {
		t.Fatalf("upload not mapped: %+v", req)
	}
	if got := str(req.Body, "bytes"); got != "png-2" {
		t.Fatalf("wrong artifact bytes: %q", got)
	}
	if !strings.Contains(out, "artifacts/deadbeef.png") || !strings.Contains(out, "cite as: run run_1") {
		t.Fatalf("upload output: %q", out)
	}

	// --kind maps onto the query; flags may follow the positional files.
	if _, _, err := runCLI(t, f, false, "journal", "upload", "run_1", shot1, "--kind", "video"); err != nil {
		t.Fatal(err)
	}
	if req := f.last(); req.Path != "/v0/journal/run_1/artifacts?kind=video&name=104-rm-today.png" {
		t.Fatalf("--kind not mapped: %+v", req)
	}

	if _, _, err := runCLI(t, f, false, "journal", "upload", "run_1"); err == nil {
		t.Fatal("expected error for missing FILE args")
	}
}

func TestJournalExport(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	// Default markdown: pages via next_seq until the run drains, renders
	// meta, the ordered step table with the failure highlighted, and the
	// artifact list.
	out, _, err := runCLI(t, f, false, "journal", "export", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## manzanasd run journal — `run_1`",
		"| Agent | agent-fake |",
		"| Entries | 3 |",
		"| Result | **FAILED** (1 of 3 steps errored) |",
		"| **error** | element not found |",
		"`artifacts/deadbeef.png` (step 3",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("markdown export missing %q:\n%s", want, out)
		}
	}

	// --format json returns the full export doc.
	out, _, err = runCLI(t, f, false, "journal", "export", "run_1", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		RunID    string           `json:"run_id"`
		Entries  []map[string]any `json:"entries"`
		Failures int              `json:"failures"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("json export not JSON: %v\n%s", err, out)
	}
	if doc.RunID != "run_1" || len(doc.Entries) != 3 || doc.Failures != 1 {
		t.Fatalf("json export wrong: %+v", doc)
	}

	// -o writes to a file instead of stdout.
	path := filepath.Join(t.TempDir(), "evidence.md")
	out, _, err = runCLI(t, f, false, "journal", "export", "run_1", "-o", path)
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("-o still wrote to stdout: %q", out)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "run journal — `run_1`") {
		t.Fatalf("-o file content wrong: %q", b)
	}

	if _, _, err := runCLI(t, f, false, "journal", "export", "run_1", "--format", "xml"); err == nil {
		t.Fatal("expected error for bad --format")
	}
}

func TestJournalExportOffline(t *testing.T) {
	// --journal-dir reads straight from the on-disk store: no daemon, and
	// the fake daemon must never see a request.
	dir := t.TempDir()
	store, err := journal.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	runID := "lse_off1"
	if err := store.WriteMeta(runID, journal.RunMeta{
		FormatVersion: journal.FormatVersion, RunID: runID, AgentID: "agent-offline",
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.Append(ctx, runID, "action", map[string]any{
		"action": "targets.boot", "status": "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, runID, "action", map[string]any{
		"action": "tap", "status": "error", "error": "sim wedged"}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	app := &appEnv{client: client.New("127.0.0.1:1"), stdout: &out, stderr: &out}
	err = cmdJournal(ctx, app, []string{"export", runID, "--journal-dir", dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| Agent | agent-offline |",
		"| Result | **FAILED** (1 of 2 steps errored) |",
		"| **error** | sim wedged |",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("offline export missing %q:\n%s", want, out.String())
		}
	}

	// Unknown run in the offline store is a clear error.
	if err := cmdJournal(ctx, app, []string{"export", "nope", "--journal-dir", dir}); err == nil {
		t.Fatal("expected error for unknown offline run")
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

func TestMCPFailsFastWhenDaemonUnreachable(t *testing.T) {
	var out, errBuf bytes.Buffer
	app := &appEnv{client: client.New("http://127.0.0.1:1"), stdout: &out, stderr: &errBuf}
	err := cmdMCP(context.Background(), app, nil)
	if err == nil || !strings.Contains(err.Error(), "MANZANASD_ADDR") ||
		!strings.Contains(err.Error(), "--daemon") {
		t.Fatalf("expected actionable fail-fast error, got %v", err)
	}
}

func TestMCPRejectsUnexpectedArgs(t *testing.T) {
	var out, errBuf bytes.Buffer
	app := &appEnv{client: client.New("http://127.0.0.1:1"), stdout: &out, stderr: &errBuf}
	err := cmdMCP(context.Background(), app, []string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "--daemon") {
		t.Fatalf("expected usage hint, got %v", err)
	}
}

func TestMCPServesOverProvidedStreams(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	var out, errBuf bytes.Buffer
	app := &appEnv{client: client.New(f.URL), stdout: &out, stderr: &errBuf}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n")
	if err := serveMCP(context.Background(), app, in, &out); err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("initialize response not JSON: %v", err)
	}
	if resp["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "manzanas" {
		t.Fatalf("bad initialize response: %v", resp)
	}
}
