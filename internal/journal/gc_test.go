package journal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGCMaxAge(t *testing.T) {
	s := testStore(t)
	backdate := backdater(t, s)
	ctx := context.Background()
	if _, err := s.Append(ctx, "old-run", "action", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "new-run", "action", nil); err != nil {
		t.Fatal(err)
	}
	backdate(filepath.Join(s.Root(), "old-run"), 48*time.Hour)

	removed, err := s.GC(GCConfig{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "old-run" {
		t.Fatalf("removed = %v", removed)
	}
	if _, err := s.Read(ctx, "new-run", 0, 0); err != nil {
		t.Fatalf("new-run gone: %v", err)
	}
}

func TestGCMaxBytesRemovesOldestFirst(t *testing.T) {
	s := testStore(t)
	backdate := backdater(t, s)
	big := strings.Repeat("x", 4096)
	for i, run := range []string{"run-1", "run-2", "run-3"} {
		if _, err := s.PutArtifact(run, "blob.bin", strings.NewReader(big)); err != nil {
			t.Fatal(err)
		}
		// Stagger mtimes so run-1 is oldest.
		backdate(filepath.Join(s.Root(), run), time.Duration(3-i)*time.Hour)
	}
	removed, err := s.GC(GCConfig{MaxBytes: 9000}) // fits two runs, not three
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "run-1" {
		t.Fatalf("removed = %v", removed)
	}
}

func TestGCNoBoundsIsNoop(t *testing.T) {
	s := testStore(t)
	if _, err := s.Append(context.Background(), "run-1", "action", nil); err != nil {
		t.Fatal(err)
	}
	removed, err := s.GC(GCConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v", removed)
	}
}

func TestGCSkipsOpenRuns(t *testing.T) {
	s := testStore(t)
	backdate := backdater(t, s)
	ctx := context.Background()
	if _, err := s.Append(ctx, "live-run", "action", nil); err != nil {
		t.Fatal(err)
	}
	s.MarkOpen("live-run")
	backdate(filepath.Join(s.Root(), "live-run"), 48*time.Hour)

	removed, err := s.GC(GCConfig{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed open run: %v", removed)
	}
	if _, err := s.Read(ctx, "live-run", 0, 0); err != nil {
		t.Fatalf("live-run gone: %v", err)
	}

	s.MarkClosed("live-run")
	removed, err = s.GC(GCConfig{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "live-run" {
		t.Fatalf("removed = %v", removed)
	}
}

func TestGCPreservesSeqCounter(t *testing.T) {
	s := testStore(t)
	backdate := backdater(t, s)
	ctx := context.Background()
	var last int64
	for i := 0; i < 3; i++ {
		ref, err := s.Append(ctx, "run-1", "action", nil)
		if err != nil {
			t.Fatal(err)
		}
		last = ref.Seq
	}
	backdate(filepath.Join(s.Root(), "run-1"), 48*time.Hour)
	removed, err := s.GC(GCConfig{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "run-1" {
		t.Fatalf("removed = %v", removed)
	}
	// The full runState is retired (bounded memory), only the high-water
	// seq is kept.
	s.mu.Lock()
	_, live := s.runs["run-1"]
	hw := s.reclaimed["run-1"]
	s.mu.Unlock()
	if live || hw != last {
		t.Fatalf("after GC: live=%v high-water=%d, want retired state with high-water %d", live, hw, last)
	}
	ref, err := s.Append(ctx, "run-1", "action", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Seq != last+1 {
		t.Fatalf("seq rewound after GC: got %d, want %d", ref.Seq, last+1)
	}
}

// backdater returns a helper that shifts every file under a dir into the
// past relative to the store's clock (age checks compare file mtimes
// against s.now, so anchoring to the wall clock would silently change the
// effective age as real time drifts from the pinned test clock).
func backdater(t *testing.T, s *FileStore) func(dir string, by time.Duration) {
	return func(dir string, by time.Duration) {
		t.Helper()
		past := s.now().Add(-by)
		err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			return os.Chtimes(path, past, past)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
