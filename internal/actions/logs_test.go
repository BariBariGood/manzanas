package actions

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

func TestLogCaptureFromPayload(t *testing.T) {
	spec, err := logCaptureFromPayload(map[string]any{})
	if err != nil || spec.enabled {
		t.Fatalf("default = %+v, %v", spec, err)
	}
	spec, err = logCaptureFromPayload(map[string]any{"capture_logs": true, "log_process": "MyApp"})
	if err != nil || !spec.enabled || spec.process != "MyApp" {
		t.Fatalf("spec = %+v, %v", spec, err)
	}
	spec, err = logCaptureFromPayload(map[string]any{"log_process": "MyApp"})
	if err != nil || !spec.enabled || spec.process != "MyApp" {
		t.Fatalf("log_process alone should imply capture: %+v, %v", spec, err)
	}
	if _, err := logCaptureFromPayload(map[string]any{"capture_logs": "yes"}); err == nil {
		t.Fatalf("non-bool capture_logs should be rejected")
	}
	if _, err := logCaptureFromPayload(map[string]any{"log_process": 42}); err == nil {
		t.Fatalf("non-string log_process should be rejected")
	}
	if _, err := logCaptureFromPayload(map[string]any{"log_process": ""}); err == nil {
		t.Fatalf("empty log_process should be rejected")
	}
}

func TestCaptureLogsAttachesToAction(t *testing.T) {
	f := newFakeRunner()
	f.stdout["tap"] = "{}"
	f.stdout["spawn"] = "12:00:00 MyApp: tapped save\n"
	b := NewAXe(WithRunner(f), WithAXePath("/fake/axe"))
	res, err := b.Dispatch(context.Background(), "UDID", proto.ActionRequest{
		Kind: "tap", Payload: map[string]any{"x": 10.0, "y": 20.0,
			"capture_logs": true, "log_process": "MyApp"}})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	logs, _ := res.Result["logs"].(string)
	if !strings.Contains(logs, "tapped save") {
		t.Fatalf("logs = %q", logs)
	}
	// The simctl invocation carries the window start and process filter.
	var spawn []string
	for _, call := range f.calls {
		if len(call) > 2 && call[2] == "spawn" {
			spawn = call
		}
	}
	joined := strings.Join(spawn, " ")
	if !strings.Contains(joined, "log show") || !strings.Contains(joined, "--start") ||
		!strings.Contains(joined, `process == "MyApp"`) {
		t.Fatalf("spawn call = %q", joined)
	}
}

func TestCaptureLogsBestEffort(t *testing.T) {
	f := newFakeRunner()
	f.stdout["tap"] = "{}"
	f.errs["spawn"] = "log: failed"
	b := NewAXe(WithRunner(f), WithAXePath("/fake/axe"))
	res, err := b.Dispatch(context.Background(), "UDID", proto.ActionRequest{
		Kind: "tap", Payload: map[string]any{"x": 10.0, "y": 20.0, "capture_logs": true}})
	if err != nil || !res.OK {
		t.Fatalf("a log failure must not fail the action: %v %+v", err, res)
	}
	if _, ok := res.Result["log_error"].(string); !ok {
		t.Fatalf("expected log_error, got %+v", res.Result)
	}
}

func TestCaptureLogsNotRequestedNoSimctl(t *testing.T) {
	f := newFakeRunner()
	f.stdout["tap"] = "{}"
	b := NewAXe(WithRunner(f), WithAXePath("/fake/axe"))
	if _, err := b.Dispatch(context.Background(), "UDID", proto.ActionRequest{
		Kind: "tap", Payload: map[string]any{"x": 10.0, "y": 20.0}}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	for _, call := range f.calls {
		if len(call) > 2 && call[2] == "spawn" {
			t.Fatalf("simctl spawn ran without capture_logs")
		}
	}
}

func TestTruncateLogTail(t *testing.T) {
	if got := truncateLogTail("short", 100); got != "short" {
		t.Fatalf("short logs must pass through, got %q", got)
	}
	long := strings.Repeat("x", 100) + "TAIL"
	got := truncateLogTail(long, 10)
	if !strings.HasSuffix(got, "TAIL") || !strings.Contains(got, "truncated") {
		t.Fatalf("truncated = %q", got)
	}
}

func TestCaptureLogsWindowStart(t *testing.T) {
	f := newFakeRunner()
	f.stdout["spawn"] = "line\n"
	b := NewAXe(WithRunner(f), WithAXePath("/fake/axe"))
	start := time.Date(2026, 8, 9, 12, 0, 30, 0, time.UTC)
	if _, err := b.captureLogs(context.Background(), "UDID", start, logCaptureSpec{enabled: true}); err != nil {
		t.Fatalf("captureLogs: %v", err)
	}
	joined := strings.Join(f.calls[0], " ")
	// One second of lead is subtracted from the action start.
	if !strings.Contains(joined, start.Add(-time.Second).Format("2006-01-02 15:04:05")) {
		t.Fatalf("start window missing lead: %q", joined)
	}
}
