// Package elementmap is the on-disk element map: named elements learned
// from successful tap_element / type_into_element resolutions, keyed by
// bundle id + device class, each carrying the frame it resolved at and a
// DHash fingerprint of the screen it was learned on. The resolver
// ladder's "map" rung serves a cached frame directly — no observe
// round-trip — when the current screen's fingerprint is close enough to
// the recorded one (see internal/actions/resolver_map.go and
// docs/elementmap.md).
//
// The store lives BESIDE the journal's runs directory (e.g.
// ~/.manzanasd/elementmaps next to ~/.manzanasd/journal), never inside a
// run dir: the map is cross-run state and the journal's GC must never
// sweep it.
//
// Layout: <root>/<bundle_id>/<class>.json — one JSON file per bundle id
// and device class holding a map of element name → Element. <class> is
// the device-class key "<backend>/<img_w>" (e.g. "axe/390",
// "mirror/318") with "/" encoded as "-" in filenames; a recorded frame
// only serves captures from the same class (pixel fingerprints do not
// survive scale changes). Element names are free-form JSON keys (the
// actions layer uses the element spec's canonical string), so they never
// touch the filesystem.
package elementmap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Frame is an element's on-screen rectangle in the coordinate space the
// element was resolved in (points on simulators, capture pixels on
// mirror-backed devices).
type Frame struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Element is one learned element.
type Element struct {
	Frame Frame  `json:"frame"`
	Role  string `json:"role,omitempty"`
	Label string `json:"label,omitempty"`
	// ScreenFingerprint is the DHash of the screen the element was
	// learned on, formatted by FormatFingerprint ("dhash:%016x").
	ScreenFingerprint string `json:"screen_fingerprint"`
	// TemplateRef optionally names a template-library entry
	// (bundle-scoped) cropped around the element.
	TemplateRef string    `json:"template_ref,omitempty"`
	LearnedAt   time.Time `json:"learned_at"`
}

// Entry is one stored element as reported by List (and the wire shape of
// the daemon's GET /v0/elementmap items).
type Entry struct {
	BundleID    string `json:"bundle_id"`
	DeviceClass string `json:"device_class"`
	Name        string `json:"name"`
	Element
}

// fingerprintPrefix tags the fingerprint encoding so a future format
// (masked variants, bigger hashes) stays distinguishable.
const fingerprintPrefix = "dhash:"

// FormatFingerprint renders a DHash for storage.
func FormatFingerprint(h uint64) string {
	return fingerprintPrefix + fmt.Sprintf("%016x", h)
}

// ParseFingerprint reads a stored fingerprint back into a DHash.
func ParseFingerprint(s string) (uint64, error) {
	hex, ok := strings.CutPrefix(s, fingerprintPrefix)
	if !ok {
		return 0, fmt.Errorf("elementmap: fingerprint %q is not %q-prefixed", s, fingerprintPrefix)
	}
	h, err := strconv.ParseUint(hex, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("elementmap: bad fingerprint %q: %w", s, err)
	}
	return h, nil
}

// Store is the on-disk element map. Safe for concurrent use.
type Store struct {
	root string
	mu   sync.Mutex
}

// NewStore opens (or creates) an element map rooted at dir.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("elementmap: create root: %w", err)
	}
	return &Store{root: dir}, nil
}

// Root returns the map's root directory.
func (s *Store) Root() string { return s.root }

// ValidateBundleID checks a bundle-id grouping key (reverse-DNS dots are
// fine).
func ValidateBundleID(bundleID string) error {
	return validateComponent("bundle_id", bundleID)
}

func validateComponent(field, v string) error {
	if v == "" {
		return fmt.Errorf("elementmap: %s must be non-empty", field)
	}
	if len(v) > 128 {
		return fmt.Errorf("elementmap: %s too long (max 128 chars)", field)
	}
	if v == "." || v == ".." {
		return fmt.Errorf("elementmap: invalid %s %q", field, v)
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return fmt.Errorf("elementmap: %s %q contains %q (allowed: letters, digits, '.', '-', '_')", field, v, string(r))
		}
	}
	return nil
}

// ValidateClass checks a device-class key: "<backend>/<img_w>".
func ValidateClass(class string) error {
	backend, width, ok := strings.Cut(class, "/")
	if !ok || backend == "" || width == "" {
		return fmt.Errorf("elementmap: device_class %q must be \"<backend>/<img_w>\"", class)
	}
	if err := validateComponent("device_class backend", backend); err != nil {
		return err
	}
	return validateComponent("device_class width", width)
}

// ValidateName checks an element name: free-form (element specs carry
// quotes and spaces), but non-empty and bounded so a hostile payload
// cannot bloat the store.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("elementmap: name must be non-empty")
	}
	if len(name) > 512 {
		return fmt.Errorf("elementmap: name too long (max 512 chars)")
	}
	return nil
}

// encodeClass maps a device-class key onto its filename form ("/" → "-").
func encodeClass(class string) string { return strings.ReplaceAll(class, "/", "-") }

// classFile returns the JSON path holding one bundle+class element map.
func (s *Store) classFile(bundleID, class string) string {
	return filepath.Join(s.root, bundleID, encodeClass(class)+".json")
}

// classDoc is the on-disk shape of one bundle+class map file. The
// device-class key is stored verbatim inside the document (the filename
// encoding is lossy: "/" and "-" collide).
type classDoc struct {
	DeviceClass string             `json:"device_class"`
	Elements    map[string]Element `json:"elements"`
}

// readClass loads one bundle+class map (empty when the file is absent).
func (s *Store) readClass(path string) (classDoc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return classDoc{Elements: map[string]Element{}}, nil
		}
		return classDoc{}, err
	}
	var doc classDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return classDoc{}, fmt.Errorf("elementmap: bad map file %s: %w", path, err)
	}
	if doc.Elements == nil {
		doc.Elements = map[string]Element{}
	}
	return doc, nil
}

// writeClass stores one bundle+class map, removing the file (and an
// emptied bundle dir) when the map is empty.
func (s *Store) writeClass(path string, doc classDoc) error {
	dir := filepath.Dir(path)
	if len(doc.Elements) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		if rest, err := os.ReadDir(dir); err == nil && len(rest) == 0 {
			_ = os.Remove(dir)
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("elementmap: create bundle dir: %w", err)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("elementmap: marshal map: %w", err)
	}
	// Write-then-rename so a crash mid-write never leaves a truncated
	// document in place.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Put records (or overwrites) one element. LearnedAt is stamped when
// zero.
func (s *Store) Put(bundleID, class, name string, el Element) error {
	if err := ValidateBundleID(bundleID); err != nil {
		return err
	}
	if err := ValidateClass(class); err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	if _, err := ParseFingerprint(el.ScreenFingerprint); err != nil {
		return err
	}
	if el.LearnedAt.IsZero() {
		el.LearnedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.classFile(bundleID, class)
	doc, err := s.readClass(path)
	if err != nil {
		// A damaged document must not permanently block learning: start
		// fresh and let the write below repair the file.
		doc = classDoc{Elements: map[string]Element{}}
	}
	doc.DeviceClass = class
	doc.Elements[name] = el
	return s.writeClass(path, doc)
}

// Get loads one element; ok reports whether it exists.
func (s *Store) Get(bundleID, class, name string) (Element, bool, error) {
	if err := ValidateBundleID(bundleID); err != nil {
		return Element{}, false, err
	}
	if err := ValidateClass(class); err != nil {
		return Element{}, false, err
	}
	if err := ValidateName(name); err != nil {
		return Element{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.readClass(s.classFile(bundleID, class))
	if err != nil {
		return Element{}, false, err
	}
	el, ok := doc.Elements[name]
	return el, ok, nil
}

// Has reports whether the bundle has the named element in ANY device
// class: the map rung's cheap pre-capture check, so a miss costs a JSON
// read instead of a screenshot.
func (s *Store) Has(bundleID, name string) bool {
	if ValidateBundleID(bundleID) != nil || ValidateName(name) != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := os.ReadDir(filepath.Join(s.root, bundleID))
	if err != nil {
		return false
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		doc, err := s.readClass(filepath.Join(s.root, bundleID, f.Name()))
		if err != nil {
			continue
		}
		if _, ok := doc.Elements[name]; ok {
			return true
		}
	}
	return false
}

// List returns every stored element, sorted by bundle, then name, then
// class. Unreadable map files are skipped.
func (s *Store) List() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bundles, err := os.ReadDir(s.root)
	if err != nil {
		// A missing root just means an empty map (e.g. cleaned up on
		// disk after the daemon started).
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
			doc, err := s.readClass(filepath.Join(s.root, b.Name(), f.Name()))
			if err != nil || doc.DeviceClass == "" {
				continue
			}
			for name, el := range doc.Elements {
				out = append(out, Entry{
					BundleID: b.Name(), DeviceClass: doc.DeviceClass, Name: name, Element: el,
				})
			}
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

// Remove deletes an element by name. An empty class removes the name
// across all device classes. Returns how many entries were removed (0
// with a nil error when nothing matched).
func (s *Store) Remove(bundleID, name, class string) (int, error) {
	if err := ValidateBundleID(bundleID); err != nil {
		return 0, err
	}
	if err := ValidateName(name); err != nil {
		return 0, err
	}
	if class != "" {
		if err := ValidateClass(class); err != nil {
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
		if class != "" && f.Name() != encodeClass(class)+".json" {
			continue
		}
		path := filepath.Join(dir, f.Name())
		doc, err := s.readClass(path)
		if err != nil {
			continue
		}
		if _, ok := doc.Elements[name]; !ok {
			continue
		}
		delete(doc.Elements, name)
		if err := s.writeClass(path, doc); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
