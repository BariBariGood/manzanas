package state

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestPackUnpackRoundTrip(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	if err := os.MkdirAll(filepath.Join(data, "Library", "Caches"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "Library", "app.db"), []byte("dbdb"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("app.db", filepath.Join(data, "Library", "link")); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(base, "device.plist")
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(base, "img.tar.zst")
	size, err := packImage(context.Background(), archive, data, plist)
	if err != nil {
		t.Fatal(err)
	}
	if size <= 0 {
		t.Fatalf("size = %d", size)
	}

	out := filepath.Join(base, "out")
	if err := unpackImage(context.Background(), archive, out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "data", "Library", "app.db"))
	if err != nil || string(b) != "dbdb" {
		t.Fatalf("file content: %v %q", err, b)
	}
	link, err := os.Readlink(filepath.Join(out, "data", "Library", "link"))
	if err != nil || link != "app.db" {
		t.Fatalf("symlink: %v %q", err, link)
	}
	// device.plist is provenance only; it must NOT be extracted.
	if _, err := os.Stat(filepath.Join(out, "device.plist")); !os.IsNotExist(err) {
		t.Fatalf("device.plist should not be extracted: %v", err)
	}
	// File modes survive.
	fi, err := os.Stat(filepath.Join(out, "data", "Library", "app.db"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", err, fi.Mode())
	}
}

func TestPackMissingPlist(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := packImage(context.Background(), filepath.Join(base, "a.tar.zst"), data, filepath.Join(base, "nope.plist")); err != nil {
		t.Fatalf("missing plist must be tolerated: %v", err)
	}
}

// writeHostileArchive writes a tar.zst with a single entry named name.
func writeHostileArchive(t *testing.T, path, name string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	body := []byte("evil")
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// writeHostileSymlinkArchive writes a tar.zst with a single symlink entry.
func writeHostileSymlinkArchive(t *testing.T, path, name, target string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeSymlink, Name: name, Linkname: target, Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUnpackRejectsHostileSymlinkTargets(t *testing.T) {
	for _, target := range []string{"/", "/etc", "../..", "../../evil", "sub/../../.."} {
		base := t.TempDir()
		archive := filepath.Join(base, "evil.tar.zst")
		writeHostileSymlinkArchive(t, archive, "data/x", target)
		err := unpackImage(context.Background(), archive, filepath.Join(base, "out"))
		if err == nil || !strings.Contains(err.Error(), "symlink target") {
			t.Fatalf("target %q: want rejection, got %v", target, err)
		}
	}
	// A relative target that stays inside data/ is fine.
	base := t.TempDir()
	archive := filepath.Join(base, "ok.tar.zst")
	writeHostileSymlinkArchive(t, archive, "data/sub/x", "../y")
	if err := unpackImage(context.Background(), archive, filepath.Join(base, "out")); err != nil {
		t.Fatalf("in-tree relative target rejected: %v", err)
	}
}

// A chain of individually in-tree-looking symlinks must not smuggle a
// link (or writes through it) outside the extraction dir: the lexical
// entry path and the real placement diverge once a path component is
// itself a symlink.
func TestUnpackRejectsChainedSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	archive := filepath.Join(base, "evil.tar.zst")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	// data/a/b/c/p -> ../../.. lexically resolves to "data" (accepted),
	// so data/a/b/c/p/q is really created at data/q; its target
	// ../../../../evil lexically resolves to data/evil but really
	// escapes the extraction dir.
	for _, hdr := range []*tar.Header{
		{Typeflag: tar.TypeDir, Name: "data/a/b/c/", Mode: 0o755},
		{Typeflag: tar.TypeSymlink, Name: "data/a/b/c/p", Linkname: "../../..", Mode: 0o777},
		{Typeflag: tar.TypeSymlink, Name: "data/a/b/c/p/q", Linkname: "../../../../evil", Mode: 0o777},
	} {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(base, "out")
	if err := unpackImage(context.Background(), archive, out); err == nil ||
		!strings.Contains(err.Error(), "image archive") {
		t.Fatalf("want rejection, got %v", err)
	}
	if _, err := os.Lstat(filepath.Join(out, "data", "q")); err == nil {
		t.Fatal("escaping symlink was created at data/q")
	}
}

func TestUnpackRejectsHostileEntries(t *testing.T) {
	for _, name := range []string{"../evil", "data/../../evil", "etc/passwd"} {
		base := t.TempDir()
		archive := filepath.Join(base, "evil.tar.zst")
		writeHostileArchive(t, archive, name)
		err := unpackImage(context.Background(), archive, filepath.Join(base, "out"))
		if err == nil || !strings.Contains(err.Error(), "image archive") {
			t.Fatalf("entry %q: want rejection, got %v", name, err)
		}
	}
}
