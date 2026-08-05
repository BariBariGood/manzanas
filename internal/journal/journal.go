// Package journal defines the interface contract for the run journal
// (actions + observations + media segment refs). The implementation is
// owned by the "journal" slice.
package journal

import (
	"context"
	"io"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// FormatVersion is the journal on-disk format version. See docs/journal.md
// for the format spec and versioning rules.
const FormatVersion = "journal/v0"

// RunMeta is the per-run determinism metadata stored in meta.json, captured
// at lease grant so a run can be replayed against an equivalent target in
// v0.2.
type RunMeta struct {
	FormatVersion string            `json:"format_version"`
	RunID         string            `json:"run_id"` // == lease ID
	LeaseID       string            `json:"lease_id,omitempty"`
	AgentID       string            `json:"agent_id,omitempty"`
	Purpose       string            `json:"purpose,omitempty"`
	TargetUDID    string            `json:"target_udid,omitempty"`
	TargetName    string            `json:"target_name,omitempty"`
	Runtime       string            `json:"runtime,omitempty"`      // e.g. "iOS 26.5"
	DeviceType    string            `json:"device_type,omitempty"`  // e.g. "iPhone 17 Pro"
	AppVersions   map[string]string `json:"app_versions,omitempty"` // bundle id -> version, when known
	DaemonVersion string            `json:"daemon_version,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
}

// RunSummary is one row of a journal listing.
type RunSummary struct {
	RunID     string    `json:"run_id"`
	LastSeq   int64     `json:"last_seq"`
	UpdatedAt time.Time `json:"updated_at"`
	Meta      RunMeta   `json:"meta"`
}

// ArtifactRef points at a stored artifact, referenced from entry payloads
// under the "artifacts" key.
type ArtifactRef struct {
	Path   string `json:"path"` // run-relative, e.g. "artifacts/ab12...ef.png"
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Entry is one journal record. Payload is opaque to the core.
type Entry struct {
	Ref     proto.JournalRef `json:"ref"`
	Kind    string           `json:"kind"` // e.g. "action", "observation", "segment"
	Payload map[string]any   `json:"payload,omitempty"`
}

// Journal appends and reads run records. Runs group entries per lease.
type Journal interface {
	// Append records an entry for a run and returns its ref.
	Append(ctx context.Context, runID string, kind string, payload map[string]any) (proto.JournalRef, error)
	// Read returns entries for a run starting at seq (inclusive).
	Read(ctx context.Context, runID string, fromSeq int64, limit int) ([]Entry, error)
}

// Store is the full journal surface the server binds to the protocol:
// Journal plus run metadata, listing, artifacts, live tailing, and evidence
// export. FileStore is the on-disk implementation.
type Store interface {
	Journal
	// WriteMeta / ReadMeta store per-run determinism metadata.
	WriteMeta(runID string, meta RunMeta) error
	ReadMeta(runID string) (RunMeta, error)
	// List summarizes all runs, newest first.
	List(ctx context.Context) ([]RunSummary, error)
	// LastSeq cheaply returns the highest seq recorded for a run.
	LastSeq(runID string) (int64, error)
	// MarkOpen flags a run as belonging to a live lease so retention/GC
	// never reclaims it; MarkClosed makes it eligible again. Call at
	// lease grant and lease end respectively.
	MarkOpen(runID string)
	MarkClosed(runID string)
	// Watch subscribes to live entries for a run (for `journal tail`).
	Watch(runID string) (<-chan Entry, func(), error)
	// PutArtifact stores content under the run's artifact dir and returns
	// its ref (relative path + sha256).
	PutArtifact(runID, name string, data io.Reader) (ArtifactRef, error)
	// OpenArtifact opens an artifact by its run-relative path.
	OpenArtifact(runID, relPath string) (io.ReadCloser, error)
	// ExportMarkdown renders a PR-comment-ready evidence summary.
	ExportMarkdown(ctx context.Context, runID string) (string, error)
}
