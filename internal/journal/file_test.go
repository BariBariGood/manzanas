package journal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func testStore(t *testing.T) *FileStore {
	t.Helper()
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) }
	return s
}

func TestAppendAssignsSequentialRefs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := int64(1); i <= 3; i++ {
		ref, err := s.Append(ctx, "run1", "action", map[string]any{"action": "tap"})
		if err != nil {
			t.Fatal(err)
		}
		if ref.RunID != "run1" || ref.Seq != i {
			t.Fatalf("ref = %+v, want run1/%d", ref, i)
		}
	}
}

func TestAppendStampsTimestamp(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Append(ctx, "run1", "action", nil); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Read(ctx, "run1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := entries[0].Payload["ts"]; got != "2026-08-01T12:00:00Z" {
		t.Fatalf("ts = %v", got)
	}
}

func TestReadSkipsOversizedLines(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Append(ctx, "run1", "action", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	// Splice an oversized line between two valid entries.
	f, err := os.OpenFile(filepath.Join(s.Root(), "run1", entriesFile), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	big := append([]byte(`{"huge":"`), make([]byte, maxLineBytes)...)
	big = append(big, []byte("\"}\n")...)
	if _, err := f.Write(big); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := s.Append(ctx, "run1", "action", map[string]any{"n": 2}); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Read(ctx, "run1", 0, 0)
	if err != nil {
		t.Fatalf("Read = %v", err)
	}
	if len(entries) != 2 || entries[0].Ref.Seq != 1 || entries[1].Ref.Seq != 2 {
		t.Fatalf("entries = %+v, want seqs 1,2", entries)
	}
}

func TestLastSeqCountsOversizedTailLine(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Append(ctx, "run1", "action", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	// Append a real oversized entry line (valid shape, huge payload) so it
	// is the newest line in the file.
	f, err := os.OpenFile(filepath.Join(s.Root(), "run1", entriesFile), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	big := []byte(`{"ref":{"run_id":"run1","seq":2},"kind":"action","payload":{"huge":"`)
	big = append(big, bytes.Repeat([]byte("x"), maxLineBytes)...)
	big = append(big, []byte("\"}}\n")...)
	if _, err := f.Write(big); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// A cold reader (fresh store over the same dir) must not undercount:
	// the oversized line's seq is recovered from its prefix.
	s2, err := NewFileStore(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	last, err := s2.LastSeq("run1")
	if err != nil || last != 2 {
		t.Fatalf("LastSeq = %d, %v, want 2", last, err)
	}
	// And a post-restart append must not collide with seq 2.
	ref, err := s2.Append(ctx, "run1", "action", map[string]any{"n": 3})
	if err != nil || ref.Seq != 3 {
		t.Fatalf("Append after restart: seq %d, %v, want 3", ref.Seq, err)
	}
}

func TestReadRefusesUnknownFormat(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Append(ctx, "run1", "action", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteMeta("run1", RunMeta{FormatVersion: "journal/v9", RunID: "run1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(ctx, "run1", 0, 0); !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("Read err = %v, want ErrUnknownFormat", err)
	}
	if _, err := s.ExportMarkdown(ctx, "run1"); !errors.Is(err, ErrUnknownFormat) {
		t.Fatalf("ExportMarkdown err = %v, want ErrUnknownFormat", err)
	}
}

func TestReadPagination(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if _, err := s.Append(ctx, "run1", "action", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	page1, err := s.Read(ctx, "run1", 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 4 || page1[0].Ref.Seq != 1 || page1[3].Ref.Seq != 4 {
		t.Fatalf("page1 = %+v", page1)
	}
	page2, err := s.Read(ctx, "run1", page1[3].Ref.Seq+1, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 4 || page2[0].Ref.Seq != 5 {
		t.Fatalf("page2 = %+v", page2)
	}
	rest, err := s.Read(ctx, "run1", 9, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 2 || rest[1].Ref.Seq != 10 {
		t.Fatalf("rest = %+v", rest)
	}
}

func TestReadUnknownRun(t *testing.T) {
	s := testStore(t)
	if _, err := s.Read(context.Background(), "nope", 0, 0); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v, want ErrRunNotFound", err)
	}
}

func TestSeqRecoveredAfterReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s1, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s1.Append(ctx, "run1", "action", nil); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := NewFileStore(dir) // simulates a daemon restart
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s2.Append(ctx, "run1", "action", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Seq != 4 {
		t.Fatalf("seq after reopen = %d, want 4", ref.Seq)
	}
}

func TestInvalidRunID(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"", "a/b", `a\b`, ".."} {
		if _, err := s.Append(context.Background(), id, "action", nil); err == nil {
			t.Fatalf("Append(%q) succeeded, want error", id)
		}
	}
}

func TestMetaRoundTrip(t *testing.T) {
	s := testStore(t)
	meta := RunMeta{
		FormatVersion: FormatVersion, RunID: "run1", LeaseID: "run1",
		AgentID: "agent-1", TargetUDID: "UDID-1", Runtime: "iOS 26.5",
		CreatedAt: time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC),
	}
	if err := s.WriteMeta("run1", meta); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadMeta("run1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, meta) {
		t.Fatalf("meta = %+v, want %+v", got, meta)
	}
}

func TestListRuns(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, run := range []string{"run-a", "run-b"} {
		if _, err := s.Append(ctx, run, "action", nil); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %+v", runs)
	}
	for _, r := range runs {
		if r.LastSeq != 1 {
			t.Fatalf("run %s LastSeq = %d", r.RunID, r.LastSeq)
		}
	}
}

func TestWatchDeliversLiveEntries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ch, cancel, err := s.Watch("run1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	ref, err := s.Append(ctx, "run1", "action", map[string]any{"action": "tap"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-ch:
		if e.Ref != ref {
			t.Fatalf("watched entry ref = %+v, want %+v", e.Ref, ref)
		}
	case <-time.After(time.Second):
		t.Fatal("no entry delivered to watcher")
	}
	cancel()
	if _, err := s.Append(ctx, "run1", "action", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case e, ok := <-ch:
		if ok {
			t.Fatalf("entry delivered after cancel: %+v", e)
		}
	default:
	}
}
