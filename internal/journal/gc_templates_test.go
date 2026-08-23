package journal

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestGCSkipsTemplatesDir pins that a "templates" directory under the
// journal root (the reserved template-library name) is never treated as a
// run and swept, however aggressive the GC bounds are.
func TestGCSkipsTemplatesDir(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// An ancient, large templates dir that would be swept if it were a run.
	tplDir := filepath.Join(dir, "templates", "com.example.app")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tpl := filepath.Join(tplDir, "login@axe-393.png")
	if err := os.WriteFile(tpl, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(tpl, old, old); err != nil {
		t.Fatal(err)
	}
	// A genuinely ancient run that must be swept.
	if _, err := s.Append(context.Background(), "run-old", "action", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	entries := filepath.Join(dir, "run-old", "entries.jsonl")
	if err := os.Chtimes(entries, old, old); err != nil {
		t.Fatal(err)
	}

	removed, err := s.GC(GCConfig{MaxAge: time.Hour, MaxBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range removed {
		if id == "templates" {
			t.Fatalf("GC removed the templates dir: %v", removed)
		}
	}
	if len(removed) != 1 || removed[0] != "run-old" {
		t.Fatalf("want [run-old] removed, got %v", removed)
	}
	if _, err := os.Stat(tpl); err != nil {
		t.Fatalf("template file swept: %v", err)
	}
}
