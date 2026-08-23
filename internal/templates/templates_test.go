package templates

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testMeta(class string) Meta {
	return Meta{
		DeviceClass: class,
		WindowPx:    [2]int{393, 852},
		CropRect:    Rect{X: 10, Y: 20, W: 30, H: 40},
		SourceRun:   "lse_abc",
		CreatedAt:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func TestPutGetListRemove(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	png := []byte("not-really-png-but-bytes")
	e, err := s.Put("com.example.app", "login-button", png, testMeta("axe/393"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Bytes != int64(len(png)) || e.SHA256 == "" {
		t.Fatalf("bad entry: %+v", e)
	}
	// Files land at <root>/<bundle>/<name>@<class-encoded>.{png,json}.
	base := filepath.Join(s.Root(), "com.example.app", "login-button@axe-393")
	for _, p := range []string{base + ".png", base + ".json"} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
	}

	got, meta, err := s.Get("com.example.app", "login-button", "axe/393")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(png) {
		t.Fatalf("roundtrip bytes mismatch")
	}
	if meta.DeviceClass != "axe/393" || meta.WindowPx != [2]int{393, 852} ||
		meta.CropRect != (Rect{X: 10, Y: 20, W: 30, H: 40}) || meta.SourceRun != "lse_abc" {
		t.Fatalf("sidecar mismatch: %+v", meta)
	}

	// A second class of the same name coexists.
	if _, err := s.Put("com.example.app", "login-button", png, testMeta("mirror/318")); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(list), list)
	}
	if list[0].DeviceClass != "axe/393" || list[1].DeviceClass != "mirror/318" {
		t.Fatalf("bad sort order: %+v", list)
	}

	// Class-scoped remove leaves the other class.
	n, err := s.Remove("com.example.app", "login-button", "axe/393")
	if err != nil || n != 1 {
		t.Fatalf("remove: n=%d err=%v", n, err)
	}
	list, _ = s.List()
	if len(list) != 1 || list[0].DeviceClass != "mirror/318" {
		t.Fatalf("after class remove: %+v", list)
	}

	// Classless remove sweeps the rest and drops the empty bundle dir.
	n, err = s.Remove("com.example.app", "login-button", "")
	if err != nil || n != 1 {
		t.Fatalf("remove all: n=%d err=%v", n, err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "com.example.app")); !os.IsNotExist(err) {
		t.Fatalf("bundle dir not cleaned up: %v", err)
	}
	if n, err := s.Remove("com.example.app", "login-button", ""); err != nil || n != 0 {
		t.Fatalf("remove missing: n=%d err=%v", n, err)
	}
}

func TestGetNotFound(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	if _, _, err := s.Get("com.example.app", "nope", "axe/393"); err == nil {
		t.Fatal("want error for missing template")
	}
}

func TestValidation(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	png := []byte("x")
	bad := []struct {
		bundle, name, class string
	}{
		{"", "n", "axe/393"},
		{"..", "n", "axe/393"},
		{"a/b", "n", "axe/393"},
		{"b", "", "axe/393"},
		{"b", "a@b", "axe/393"},
		{"b", "a/b", "axe/393"},
		{"b", "n", "no-slash"},
		{"b", "n", "/393"},
		{"b", "n", "axe/"},
	}
	for _, c := range bad {
		if _, err := s.Put(c.bundle, c.name, png, testMeta(c.class)); err == nil {
			t.Errorf("Put(%q,%q,%q): want validation error", c.bundle, c.name, c.class)
		}
	}
	if _, err := s.Put("b", "n", nil, testMeta("axe/393")); err == nil {
		t.Error("Put with empty image: want error")
	}
}
