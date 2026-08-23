// Package templates is the on-disk template library: named PNG crops of
// on-screen elements, saved by the save_template action and matched by
// later template actions. The store lives BESIDE the journal's runs
// directory (e.g. ~/.manzanasd/templates next to ~/.manzanasd/journal),
// never inside a run dir: templates are cross-run state and the journal's
// GC must never sweep them.
//
// Layout: <root>/<bundle_id>/<name>@<class>.png plus a JSON sidecar
// (<name>@<class>.json) carrying the capture metadata. <class> is the
// device-class key — "<backend>/<img_w>" (e.g. "mirror/318", "axe/393")
// — with "/" encoded as "-" in filenames; a template only matches frames
// from the same class (pixel templates do not survive scale changes).
package templates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Rect is a pixel rectangle in capture-image coordinates.
type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Meta is the JSON sidecar stored next to each template PNG.
type Meta struct {
	// DeviceClass keys the capture scale: "<backend>/<img_w>".
	DeviceClass string `json:"device_class"`
	// WindowPx is the full capture size [w, h] the crop was taken from.
	WindowPx [2]int `json:"window_px"`
	// CropRect is where in the capture the template was cropped.
	CropRect Rect `json:"crop_rect"`
	// SourceRun is the journal run the save was journaled under.
	SourceRun string `json:"source_run,omitempty"`
	// MinScoreSeen is the lowest successful match score recorded by
	// template-matching actions (0 until a match updates it).
	MinScoreSeen float64   `json:"min_score_seen,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// Entry is one stored template as reported by List (and the wire shape of
// the daemon's GET /v0/templates items).
type Entry struct {
	BundleID string `json:"bundle_id"`
	Name     string `json:"name"`
	Meta
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Store is the on-disk template library. Safe for concurrent use.
type Store struct {
	root string
	mu   sync.Mutex
}

// NewStore opens (or creates) a template library rooted at dir.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("templates: create root: %w", err)
	}
	return &Store{root: dir}, nil
}

// Root returns the library's root directory.
func (s *Store) Root() string { return s.root }

// ValidateName checks a template name: a single safe path component with
// no "@" (the name/class delimiter in filenames).
func ValidateName(name string) error {
	return validateComponent("name", name)
}

// ValidateBundleID checks a bundle-id grouping key (same rules as names;
// reverse-DNS dots are fine).
func ValidateBundleID(bundleID string) error {
	return validateComponent("bundle_id", bundleID)
}

func validateComponent(field, v string) error {
	if v == "" {
		return fmt.Errorf("templates: %s must be non-empty", field)
	}
	if len(v) > 128 {
		return fmt.Errorf("templates: %s too long (max 128 chars)", field)
	}
	if v == "." || v == ".." {
		return fmt.Errorf("templates: invalid %s %q", field, v)
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return fmt.Errorf("templates: %s %q contains %q (allowed: letters, digits, '.', '-', '_')", field, v, string(r))
		}
	}
	return nil
}

// ValidateClass checks a device-class key: "<backend>/<img_w>".
func ValidateClass(class string) error {
	return validateClass(class)
}

// validateClass checks a device-class key: "<backend>/<img_w>".
func validateClass(class string) error {
	backend, width, ok := strings.Cut(class, "/")
	if !ok || backend == "" || width == "" {
		return fmt.Errorf("templates: device_class %q must be \"<backend>/<img_w>\"", class)
	}
	if err := validateComponent("device_class backend", backend); err != nil {
		return err
	}
	return validateComponent("device_class width", width)
}

// encodeClass maps a device-class key onto its filename form ("/" → "-").
func encodeClass(class string) string { return strings.ReplaceAll(class, "/", "-") }

// paths returns the PNG and sidecar paths for one template.
func (s *Store) paths(bundleID, name, class string) (png, sidecar string) {
	base := filepath.Join(s.root, bundleID, name+"@"+encodeClass(class))
	return base + ".png", base + ".json"
}

// Put stores (or overwrites) one template: the PNG crop plus its sidecar.
// meta.DeviceClass keys the file; CreatedAt is stamped when zero.
func (s *Store) Put(bundleID, name string, png []byte, meta Meta) (Entry, error) {
	if err := ValidateBundleID(bundleID); err != nil {
		return Entry{}, err
	}
	if err := ValidateName(name); err != nil {
		return Entry{}, err
	}
	if err := validateClass(meta.DeviceClass); err != nil {
		return Entry{}, err
	}
	if len(png) == 0 {
		return Entry{}, fmt.Errorf("templates: empty image")
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(s.root, bundleID), 0o755); err != nil {
		return Entry{}, fmt.Errorf("templates: create bundle dir: %w", err)
	}
	pngPath, sidecarPath := s.paths(bundleID, name, meta.DeviceClass)
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return Entry{}, fmt.Errorf("templates: marshal sidecar: %w", err)
	}
	if err := os.WriteFile(pngPath, png, 0o644); err != nil {
		return Entry{}, fmt.Errorf("templates: write png: %w", err)
	}
	if err := os.WriteFile(sidecarPath, raw, 0o644); err != nil {
		// Never leave a PNG without its sidecar: List/Get treat the
		// sidecar as the source of truth.
		_ = os.Remove(pngPath)
		return Entry{}, fmt.Errorf("templates: write sidecar: %w", err)
	}
	sum := sha256.Sum256(png)
	return Entry{
		BundleID: bundleID, Name: name, Meta: meta,
		Bytes: int64(len(png)), SHA256: hex.EncodeToString(sum[:]),
	}, nil
}

// Get loads one template's PNG bytes and sidecar.
func (s *Store) Get(bundleID, name, class string) ([]byte, Meta, error) {
	if err := ValidateBundleID(bundleID); err != nil {
		return nil, Meta{}, err
	}
	if err := ValidateName(name); err != nil {
		return nil, Meta{}, err
	}
	if err := validateClass(class); err != nil {
		return nil, Meta{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pngPath, sidecarPath := s.paths(bundleID, name, class)
	raw, err := os.ReadFile(sidecarPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, Meta{}, fmt.Errorf("templates: %s/%s@%s not found", bundleID, name, class)
		}
		return nil, Meta{}, err
	}
	var meta Meta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, Meta{}, fmt.Errorf("templates: bad sidecar %s: %w", sidecarPath, err)
	}
	png, err := os.ReadFile(pngPath)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("templates: read png: %w", err)
	}
	return png, meta, nil
}

// HasNamed reports whether any device class of bundleID/name is
// stored — a directory-listing existence probe, cheap enough to run
// before paying for a screen capture. Invalid keys simply report false.
func (s *Store) HasNamed(bundleID, name string) bool {
	if ValidateBundleID(bundleID) != nil || ValidateName(name) != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(s.root, bundleID))
	if err != nil {
		return false
	}
	prefix := name + "@"
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".png") {
			return true
		}
	}
	return false
}

// Has reports whether bundleID/name is stored for one specific device
// class — a stat-level existence probe. Invalid keys report false.
func (s *Store) Has(bundleID, name, class string) bool {
	if ValidateBundleID(bundleID) != nil || ValidateName(name) != nil || validateClass(class) != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pngPath, _ := s.paths(bundleID, name, class)
	fi, err := os.Stat(pngPath)
	return err == nil && !fi.IsDir()
}

// RecordScore records a successful match score on a template's sidecar:
// min_score_seen keeps the lowest score that still counted as a match,
// a drift signal for threshold tuning. Scores at or above the current
// minimum are no-ops.
func (s *Store) RecordScore(bundleID, name, class string, score float64) error {
	if err := ValidateBundleID(bundleID); err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := validateClass(class); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, sidecarPath := s.paths(bundleID, name, class)
	raw, err := os.ReadFile(sidecarPath)
	if err != nil {
		return fmt.Errorf("templates: read sidecar: %w", err)
	}
	var meta Meta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return fmt.Errorf("templates: bad sidecar %s: %w", sidecarPath, err)
	}
	if meta.MinScoreSeen != 0 && score >= meta.MinScoreSeen {
		return nil
	}
	meta.MinScoreSeen = score
	out, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("templates: marshal sidecar: %w", err)
	}
	return os.WriteFile(sidecarPath, out, 0o644)
}

// List returns every stored template, sorted by bundle then name then
// class. Files without a readable sidecar are skipped.
func (s *Store) List() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bundles, err := os.ReadDir(s.root)
	if err != nil {
		// A missing root just means an empty library (e.g. cleaned up
		// on disk after the daemon started).
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Entry
	for _, b := range bundles {
		if !b.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.root, b.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			base := strings.TrimSuffix(f.Name(), ".json")
			name, _, ok := strings.Cut(base, "@")
			if !ok {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(s.root, b.Name(), f.Name()))
			if err != nil {
				continue
			}
			var meta Meta
			if err := json.Unmarshal(raw, &meta); err != nil {
				continue
			}
			e := Entry{BundleID: b.Name(), Name: name, Meta: meta}
			if png, err := os.ReadFile(filepath.Join(s.root, b.Name(), base+".png")); err == nil {
				sum := sha256.Sum256(png)
				e.Bytes = int64(len(png))
				e.SHA256 = hex.EncodeToString(sum[:])
			}
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BundleID != out[j].BundleID {
			return out[i].BundleID < out[j].BundleID
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].DeviceClass < out[j].DeviceClass
	})
	return out, nil
}

// Remove deletes a template (PNG + sidecar). An empty class removes the
// name across all device classes. Returns how many templates were removed
// (0 with a nil error when nothing matched).
func (s *Store) Remove(bundleID, name, class string) (int, error) {
	if err := ValidateBundleID(bundleID); err != nil {
		return 0, err
	}
	if err := ValidateName(name); err != nil {
		return 0, err
	}
	if class != "" {
		if err := validateClass(class); err != nil {
			return 0, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.root, bundleID)
	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		base := strings.TrimSuffix(f.Name(), ".json")
		n, enc, ok := strings.Cut(base, "@")
		if !ok || n != name {
			continue
		}
		if class != "" && enc != encodeClass(class) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, f.Name())); err != nil {
			return removed, err
		}
		_ = os.Remove(filepath.Join(dir, base+".png"))
		removed++
	}
	// Drop the bundle dir once its last template is gone.
	if removed > 0 {
		if rest, err := os.ReadDir(dir); err == nil && len(rest) == 0 {
			_ = os.Remove(dir)
		}
	}
	return removed, nil
}
