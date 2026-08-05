package state

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// protectedTree builds a small tree with one file the process cannot
// read, mimicking a booted sim's system-protected caches (locationd
// locScoreInfo files deny reads with EPERM even to the owner).
func protectedTree(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: cannot create unreadable files")
	}
	src := filepath.Join(t.TempDir(), "data")
	cacheDir := filepath.Join(src, "Library", "Caches", "locationd", "locScoreInfo")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(cacheDir, "secondary-1")
	if err := os.WriteFile(locked, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	return src
}

func TestCopyTreeTolerantSkipsUnreadable(t *testing.T) {
	src := protectedTree(t)
	dst := filepath.Join(t.TempDir(), "copy")
	skipped, err := copyTreeTolerant(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("copyTreeTolerant: %v", err)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
	got, err := os.ReadFile(filepath.Join(dst, "keep.txt"))
	if err != nil || string(got) != "keep" {
		t.Fatalf("keep.txt = %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "Library", "Caches", "locationd", "locScoreInfo", "secondary-1")); !os.IsNotExist(err) {
		t.Fatalf("unreadable file should be absent from copy, got err=%v", err)
	}
}

func TestReplaceDirFallsBackWhenCpFails(t *testing.T) {
	src := protectedTree(t)
	dst := filepath.Join(filepath.Dir(src), "restored")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "dirty.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewHostRunner("")
	if err := r.ReplaceDir(context.Background(), src, dst); err != nil {
		t.Fatalf("ReplaceDir: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "keep.txt"))
	if err != nil || string(got) != "keep" {
		t.Fatalf("keep.txt = %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "dirty.txt")); !os.IsNotExist(err) {
		t.Fatalf("dirty.txt should be gone after replace, got err=%v", err)
	}
}

func TestTarTreeSkipsUnreadable(t *testing.T) {
	src := protectedTree(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tarTree(context.Background(), tw, src, "data"); err != nil {
		t.Fatalf("tarTree: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[hdr.Name] = true
	}
	if !names["data/keep.txt"] {
		t.Fatal("keep.txt missing from archive")
	}
	if names["data/Library/Caches/locationd/locScoreInfo/secondary-1"] {
		t.Fatal("unreadable file should be skipped, not archived")
	}
}
