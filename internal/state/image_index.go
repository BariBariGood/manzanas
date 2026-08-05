package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/BariBariGood/manzanas/proto"
)

// imageIndex is a small JSON-backed index of golden images on this host
// (mirrors snapshotIndex).
type imageIndex struct {
	mu   sync.Mutex
	path string
}

type imageIndexFile struct {
	Images []proto.ImageInfo `json:"images"`
}

func newImageIndex(path string) *imageIndex {
	return &imageIndex{path: path}
}

func (x *imageIndex) load() (imageIndexFile, error) {
	var f imageIndexFile
	b, err := os.ReadFile(x.path)
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("read image index: %w", err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("parse image index %s: %w", x.path, err)
	}
	return f, nil
}

func (x *imageIndex) save(f imageIndexFile) error {
	if err := os.MkdirAll(filepath.Dir(x.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := x.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, x.path)
}

// List returns all images.
func (x *imageIndex) List() ([]proto.ImageInfo, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	f, err := x.load()
	if err != nil {
		return nil, err
	}
	return f.Images, nil
}

// Add appends an image to the index.
func (x *imageIndex) Add(img proto.ImageInfo) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	f, err := x.load()
	if err != nil {
		return err
	}
	f.Images = append(f.Images, img)
	return x.save(f)
}

// Remove deletes an image by ID, returning it.
func (x *imageIndex) Remove(id string) (proto.ImageInfo, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	f, err := x.load()
	if err != nil {
		return proto.ImageInfo{}, err
	}
	for i, img := range f.Images {
		if img.ID == id {
			f.Images = append(f.Images[:i], f.Images[i+1:]...)
			return img, x.save(f)
		}
	}
	return proto.ImageInfo{}, ErrImageNotFound
}

// Resolve finds an image by ID, or by name (the most recently created
// match wins).
func (x *imageIndex) Resolve(idOrName string) (proto.ImageInfo, error) {
	// A blank identifier must never match an unnamed image.
	if idOrName == "" {
		return proto.ImageInfo{}, ErrImageNotFound
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	f, err := x.load()
	if err != nil {
		return proto.ImageInfo{}, err
	}
	var best *proto.ImageInfo
	for i := range f.Images {
		img := &f.Images[i]
		if img.ID == idOrName {
			return checkedImage(*img)
		}
		if img.Name == idOrName {
			if best == nil || img.CreatedAt.After(best.CreatedAt) {
				best = img
			}
		}
	}
	if best != nil {
		return checkedImage(*best)
	}
	return proto.ImageInfo{}, ErrImageNotFound
}

// imageIDRe matches daemon-generated image IDs (newImageID). Resolved
// IDs become filesystem paths (archivePath), so an ID read back from a
// corrupted or hand-edited index must never smuggle path components.
var imageIDRe = regexp.MustCompile(`^img_[0-9a-f]{12}$`)

func checkedImage(img proto.ImageInfo) (proto.ImageInfo, error) {
	if !imageIDRe.MatchString(img.ID) {
		return proto.ImageInfo{}, fmt.Errorf("image index: malformed image id %q", img.ID)
	}
	return img, nil
}
