package elementmap

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testElement(fp uint64) Element {
	return Element{
		Frame:             Frame{X: 10, Y: 20, W: 100, H: 44},
		Role:              "Button",
		Label:             "Sign In",
		ScreenFingerprint: FormatFingerprint(fp),
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := `label == "Sign In"`
	if err := st.Put("com.example.app", "axe/390", name, testElement(0xabc)); err != nil {
		t.Fatal(err)
	}
	el, ok, err := st.Get("com.example.app", "axe/390", name)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if el.Frame != (Frame{X: 10, Y: 20, W: 100, H: 44}) || el.Role != "Button" || el.Label != "Sign In" {
		t.Fatalf("bad element: %+v", el)
	}
	fp, err := ParseFingerprint(el.ScreenFingerprint)
	if err != nil || fp != 0xabc {
		t.Fatalf("fingerprint: %v %v", fp, err)
	}
	if el.LearnedAt.IsZero() {
		t.Fatal("LearnedAt not stamped")
	}
	if _, ok, _ := st.Get("com.example.app", "axe/999", name); ok {
		t.Fatal("Get hit the wrong device class")
	}
	if _, ok, _ := st.Get("com.example.app", "axe/390", "other"); ok {
		t.Fatal("Get hit the wrong name")
	}
}

func TestPutOverwrites(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put("com.example.app", "axe/390", "n", testElement(1)); err != nil {
		t.Fatal(err)
	}
	el2 := testElement(2)
	el2.Frame.X = 99
	if err := st.Put("com.example.app", "axe/390", "n", el2); err != nil {
		t.Fatal(err)
	}
	el, ok, err := st.Get("com.example.app", "axe/390", "n")
	if err != nil || !ok || el.Frame.X != 99 || el.ScreenFingerprint != FormatFingerprint(2) {
		t.Fatalf("overwrite lost: ok=%v err=%v %+v", ok, err, el)
	}
	list, err := st.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List after overwrite: %v %v", list, err)
	}
}

func TestHas(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if st.Has("com.example.app", "n") {
		t.Fatal("Has on empty store")
	}
	if err := st.Put("com.example.app", "mirror/318", "n", testElement(7)); err != nil {
		t.Fatal(err)
	}
	if !st.Has("com.example.app", "n") {
		t.Fatal("Has missed a stored element")
	}
	if st.Has("com.other.app", "n") {
		t.Fatal("Has crossed bundles")
	}
}

func TestListSortedAcrossBundlesAndClasses(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	puts := []struct{ bundle, class, name string }{
		{"com.b", "axe/390", "z"},
		{"com.a", "mirror/318", "n"},
		{"com.a", "axe/390", "n"},
	}
	for _, p := range puts {
		if err := st.Put(p.bundle, p.class, p.name, testElement(3)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := st.List()
	if err != nil || len(list) != 3 {
		t.Fatalf("List: %v %v", list, err)
	}
	want := []struct{ bundle, class, name string }{
		{"com.a", "axe/390", "n"},
		{"com.a", "mirror/318", "n"},
		{"com.b", "axe/390", "z"},
	}
	for i, w := range want {
		e := list[i]
		if e.BundleID != w.bundle || e.DeviceClass != w.class || e.Name != w.name {
			t.Fatalf("list[%d] = %+v, want %+v", i, e, w)
		}
	}
}

func TestRemoveScopedAndAllClasses(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"axe/390", "mirror/318"} {
		if err := st.Put("com.example.app", class, "n", testElement(4)); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := st.Remove("com.example.app", "n", "axe/390")
	if err != nil || removed != 1 {
		t.Fatalf("scoped remove: %d %v", removed, err)
	}
	if _, ok, _ := st.Get("com.example.app", "mirror/318", "n"); !ok {
		t.Fatal("scoped remove swept the other class")
	}
	removed, err = st.Remove("com.example.app", "n", "")
	if err != nil || removed != 1 {
		t.Fatalf("all-class remove: %d %v", removed, err)
	}
	removed, err = st.Remove("com.example.app", "n", "")
	if err != nil || removed != 0 {
		t.Fatalf("second remove: %d %v", removed, err)
	}
	// The emptied bundle dir is cleaned up.
	if _, err := os.Stat(filepath.Join(st.Root(), "com.example.app")); !os.IsNotExist(err) {
		t.Fatalf("bundle dir survived: %v", err)
	}
}

func TestPutRepairsDamagedFile(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put("com.example.app", "axe/390", "n", testElement(8)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(st.Root(), "com.example.app", "axe-390.json")
	if err := os.WriteFile(path, []byte("{truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := st.Put("com.example.app", "axe/390", "n", testElement(9)); err != nil {
		t.Fatalf("Put on damaged file: %v", err)
	}
	el, ok, err := st.Get("com.example.app", "axe/390", "n")
	if err != nil || !ok || el.ScreenFingerprint != FormatFingerprint(9) {
		t.Fatalf("damaged file not repaired: ok=%v err=%v %+v", ok, err, el)
	}
}

func TestValidation(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	el := testElement(5)
	if err := st.Put("../evil", "axe/390", "n", el); err == nil {
		t.Fatal("bad bundle accepted")
	}
	if err := st.Put("com.ok", "no-slash", "n", el); err == nil {
		t.Fatal("bad class accepted")
	}
	if err := st.Put("com.ok", "axe/390", "", el); err == nil {
		t.Fatal("empty name accepted")
	}
	bad := el
	bad.ScreenFingerprint = "nonsense"
	if err := st.Put("com.ok", "axe/390", "n", bad); err == nil {
		t.Fatal("bad fingerprint accepted")
	}
}

func TestFingerprintRoundTrip(t *testing.T) {
	for _, h := range []uint64{0, 1, 0xdeadbeefcafef00d, ^uint64(0)} {
		got, err := ParseFingerprint(FormatFingerprint(h))
		if err != nil || got != h {
			t.Fatalf("round-trip %x: %x %v", h, got, err)
		}
	}
	if _, err := ParseFingerprint("0123456789abcdef"); err == nil {
		t.Fatal("unprefixed fingerprint accepted")
	}
}

func TestLearnedAtPreserved(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	el := testElement(6)
	el.LearnedAt = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := st.Put("com.example.app", "axe/390", "n", el); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.Get("com.example.app", "axe/390", "n")
	if err != nil || !ok || !got.LearnedAt.Equal(el.LearnedAt) {
		t.Fatalf("LearnedAt: %v %v %v", got.LearnedAt, ok, err)
	}
}
