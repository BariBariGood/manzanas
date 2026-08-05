package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// PutArtifact stores content under <run>/artifacts/, content-addressed as
// <sha256><ext> where ext comes from the supplied name. Duplicate content
// dedupes to the same file.
func (s *FileStore) PutArtifact(runID, name string, data io.Reader) (ArtifactRef, error) {
	_, dir, err := s.state(runID)
	if err != nil {
		return ArtifactRef{}, err
	}
	adir := filepath.Join(dir, artifactsDir)
	if err := os.MkdirAll(adir, 0o755); err != nil {
		return ArtifactRef{}, fmt.Errorf("journal: create artifacts dir: %w", err)
	}
	tmp, err := os.CreateTemp(adir, ".upload-*")
	if err != nil {
		return ArtifactRef{}, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("journal: write artifact: %w", err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	fname := sum + safeExt(name)
	final := filepath.Join(adir, fname)
	if _, statErr := os.Stat(final); statErr != nil {
		if err := os.Rename(tmp.Name(), final); err != nil {
			return ArtifactRef{}, fmt.Errorf("journal: store artifact: %w", err)
		}
	}
	return ArtifactRef{Path: artifactsDir + "/" + fname, SHA256: sum, Bytes: n}, nil
}

// OpenArtifact opens an artifact by its run-relative path (as returned in
// ArtifactRef.Path).
func (s *FileStore) OpenArtifact(runID, relPath string) (io.ReadCloser, error) {
	dir, err := s.runDir(runID)
	if err != nil {
		return nil, err
	}
	clean := filepath.Clean(relPath)
	if clean != relPath && clean+"/" != relPath {
		return nil, fmt.Errorf("journal: invalid artifact path %q", relPath)
	}
	if !strings.HasPrefix(clean, artifactsDir+string(filepath.Separator)) &&
		!strings.HasPrefix(clean, artifactsDir+"/") {
		return nil, fmt.Errorf("journal: invalid artifact path %q", relPath)
	}
	f, err := os.Open(filepath.Join(dir, clean))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	return f, nil
}

// safeExt extracts a filesystem-safe extension (with dot) from a name.
func safeExt(name string) string {
	ext := strings.ToLower(filepath.Ext(filepath.Base(name)))
	if len(ext) < 2 || len(ext) > 16 {
		return ""
	}
	for _, r := range ext[1:] {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return ""
		}
	}
	return ext
}
