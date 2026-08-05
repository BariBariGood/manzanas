package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/BariBariGood/manzanas/proto"
)

// snapshotIndex is a small JSON-backed index of snapshots on this host.
type snapshotIndex struct {
	mu   sync.Mutex
	path string
}

type indexFile struct {
	Snapshots []proto.SnapshotInfo `json:"snapshots"`
}

func newSnapshotIndex(path string) *snapshotIndex {
	return &snapshotIndex{path: path}
}

func (x *snapshotIndex) load() (indexFile, error) {
	var f indexFile
	b, err := os.ReadFile(x.path)
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("read snapshot index: %w", err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("parse snapshot index %s: %w", x.path, err)
	}
	return f, nil
}

func (x *snapshotIndex) save(f indexFile) error {
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

// List returns all snapshots.
func (x *snapshotIndex) List() ([]proto.SnapshotInfo, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	f, err := x.load()
	if err != nil {
		return nil, err
	}
	return f.Snapshots, nil
}

// Add appends a snapshot to the index.
func (x *snapshotIndex) Add(s proto.SnapshotInfo) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	f, err := x.load()
	if err != nil {
		return err
	}
	f.Snapshots = append(f.Snapshots, s)
	return x.save(f)
}

// Remove deletes a snapshot by ID, returning it.
func (x *snapshotIndex) Remove(id string) (proto.SnapshotInfo, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	f, err := x.load()
	if err != nil {
		return proto.SnapshotInfo{}, err
	}
	for i, s := range f.Snapshots {
		if s.ID == id {
			f.Snapshots = append(f.Snapshots[:i], f.Snapshots[i+1:]...)
			return s, x.save(f)
		}
	}
	return proto.SnapshotInfo{}, ErrSnapshotNotFound
}

// Resolve finds a snapshot by ID, or by label scoped to a source UDID
// (the most recently created match wins).
func (x *snapshotIndex) Resolve(udid, idOrLabel string) (proto.SnapshotInfo, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	f, err := x.load()
	if err != nil {
		return proto.SnapshotInfo{}, err
	}
	var best *proto.SnapshotInfo
	for i := range f.Snapshots {
		s := &f.Snapshots[i]
		if s.ID == idOrLabel {
			if udid != "" && s.SourceUDID != udid {
				// Deliberately not_found (and not naming the owner): a
				// snapshot from another target doesn't exist as far as this
				// lease is concerned.
				return proto.SnapshotInfo{}, fmt.Errorf("%w: %s was taken from another target", ErrSnapshotNotFound, s.ID)
			}
			return *s, nil
		}
		if s.Label == idOrLabel && (udid == "" || s.SourceUDID == udid) {
			if best == nil || s.CreatedAt.After(best.CreatedAt) {
				best = s
			}
		}
	}
	if best != nil {
		return *best, nil
	}
	return proto.SnapshotInfo{}, ErrSnapshotNotFound
}
