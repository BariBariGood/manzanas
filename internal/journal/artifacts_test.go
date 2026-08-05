package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

func TestPutArtifactHashesContent(t *testing.T) {
	s := testStore(t)
	content := "pretend this is a PNG"
	ref, err := s.PutArtifact("run1", "screenshot.png", strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	want := hex.EncodeToString(sum[:])
	if ref.SHA256 != want {
		t.Fatalf("sha256 = %s, want %s", ref.SHA256, want)
	}
	if ref.Path != "artifacts/"+want+".png" {
		t.Fatalf("path = %s", ref.Path)
	}
	if ref.Bytes != int64(len(content)) {
		t.Fatalf("bytes = %d", ref.Bytes)
	}
}

func TestArtifactRoundTrip(t *testing.T) {
	s := testStore(t)
	ref, err := s.PutArtifact("run1", "shot.png", strings.NewReader("bytes"))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := s.OpenArtifact("run1", ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "bytes" {
		t.Fatalf("content = %q", got)
	}
}

func TestPutArtifactDedupes(t *testing.T) {
	s := testStore(t)
	r1, err := s.PutArtifact("run1", "a.png", strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.PutArtifact("run1", "b.png", strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	if r1.Path != r2.Path || r1.SHA256 != r2.SHA256 {
		t.Fatalf("refs differ: %+v vs %+v", r1, r2)
	}
}

func TestOpenArtifactRejectsTraversal(t *testing.T) {
	s := testStore(t)
	if _, err := s.PutArtifact("run1", "a.png", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../run2/artifacts/x", "artifacts/../meta.json", "meta.json", "/etc/passwd"} {
		if _, err := s.OpenArtifact("run1", p); err == nil {
			t.Fatalf("OpenArtifact(%q) succeeded, want error", p)
		}
	}
}

func TestSafeExt(t *testing.T) {
	cases := map[string]string{
		"shot.PNG":               ".png",
		"video.mp4":              ".mp4",
		"noext":                  "",
		"weird.p n g":            "",
		"a.":                     "",
		"x.wayyyyyyyytoolongext": "",
	}
	for in, want := range cases {
		if got := safeExt(in); got != want {
			t.Errorf("safeExt(%q) = %q, want %q", in, got, want)
		}
	}
}
