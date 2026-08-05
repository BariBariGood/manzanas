package journal

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestExportMarkdownGolden(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	runID := "lse_smoke1"

	if err := s.WriteMeta(runID, RunMeta{
		FormatVersion: FormatVersion, RunID: runID, LeaseID: runID,
		AgentID: "agent-journal", Purpose: "slice-5 smoke test",
		TargetUDID: "AAAA-1111", TargetName: "iPhone 17 Pro",
		Runtime: "iOS 26.5", DeviceType: "iPhone 17 Pro",
		DaemonVersion: "v0",
		CreatedAt:     time.Date(2026, 8, 1, 11, 59, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	rec := NewRecorder(s)
	rec.Record(ctx, Event{Kind: "lease", LeaseID: runID, AgentID: "agent-journal",
		Action: "leases.acquire", Status: "ok",
		Params: map[string]any{"labels": []string{"ios26"}, "ttl_seconds": 300}})
	rec.Record(ctx, Event{Kind: "action", LeaseID: runID, Action: "targets.boot",
		Params: map[string]any{"udid": "AAAA-1111"}, Status: "ok"})
	rec.Record(ctx, Event{Kind: "observation", LeaseID: runID, Action: "journal.artifact",
		Params: map[string]any{"name": "boot.png"}, Status: "ok",
		Artifacts: []ArtifactRef{{Path: "artifacts/deadbeef00112233.png",
			SHA256: "deadbeef001122334455667788990011deadbeef001122334455667788990011", Bytes: 1234}}})
	rec.Record(ctx, Event{Kind: "segment", LeaseID: runID, Action: "recording.stop",
		Params: map[string]any{"udid": "AAAA-1111", "codec": "hevc",
			"duration_s": 12.5, "bytes": float64(2 << 20), "reason": "stopped"},
		Status: "ok",
		Artifacts: []ArtifactRef{{Path: "artifacts/cafef00d8899aabb.mp4",
			SHA256: "cafef00d8899aabbccddeeff00112233cafef00d8899aabbccddeeff00112233", Bytes: 2 << 20}}})
	rec.Record(ctx, Event{Kind: "action", LeaseID: runID, Action: "targets.shutdown",
		Params: map[string]any{"udid": "AAAA-1111"}, Status: "error", Error: "sim wedged"})
	rec.Record(ctx, Event{Kind: "lease", LeaseID: runID, AgentID: "agent-journal",
		Action: "leases.release", Status: "ok"})

	got, err := s.ExportMarkdown(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "export_smoke.golden.md")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("markdown mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestExportMarkdownUnknownRun(t *testing.T) {
	s := testStore(t)
	if _, err := s.ExportMarkdown(context.Background(), "nope"); err == nil {
		t.Fatal("want error for unknown run")
	}
}
