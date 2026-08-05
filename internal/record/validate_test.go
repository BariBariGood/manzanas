package record

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidateMP4Valid(t *testing.T) {
	if err := ValidateMP4(writeTemp(t, validMP4)); err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
}

func TestValidateMP4ZeroByte(t *testing.T) {
	err := ValidateMP4(writeTemp(t, nil))
	if !errors.Is(err, ErrInvalidRecording) {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateMP4NoMoov(t *testing.T) {
	// ftyp box only, no moov.
	err := ValidateMP4(writeTemp(t, []byte("\x00\x00\x00\x10ftypisom\x00\x00\x00\x00")))
	if !errors.Is(err, ErrInvalidRecording) {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateMP4Garbage(t *testing.T) {
	err := ValidateMP4(writeTemp(t, []byte("not an mp4 at all, just text")))
	if !errors.Is(err, ErrInvalidRecording) {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateMP4LargesizeAndToEOF(t *testing.T) {
	// A 64-bit largesize ftyp box followed by a size-0 (to-EOF) moov box.
	var buf []byte
	large := make([]byte, 24)
	binary.BigEndian.PutUint32(large[0:4], 1)
	copy(large[4:8], "ftyp")
	binary.BigEndian.PutUint64(large[8:16], 24)
	buf = append(buf, large...)
	tail := make([]byte, 12)
	copy(tail[4:8], "moov") // size 0 => extends to EOF
	buf = append(buf, tail...)
	if err := ValidateMP4(writeTemp(t, buf)); err != nil {
		t.Fatalf("err = %v", err)
	}
}
