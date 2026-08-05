package server

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/proto"
)

// maxArtifactBytes bounds a single artifact upload (64 MiB).
const maxArtifactBytes = 64 << 20

// journalStore returns the store, or writes 501 and nil when the journal
// slice is not wired in this build.
func (s *Server) journalStore(w http.ResponseWriter) journal.Store {
	store := s.journal.Store()
	if store == nil {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented,
			"journal is not implemented in this build")
	}
	return store
}

// GET /v0/journal — list runs.
func (s *Server) handleJournalList(w http.ResponseWriter, r *http.Request) {
	store := s.journalStore(w)
	if store == nil {
		return
	}
	runs, err := store.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	if runs == nil {
		runs = []journal.RunSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// GET /v0/journal/{run}?from_seq=&limit= — paginated entries + meta.
func (s *Server) handleJournalGet(w http.ResponseWriter, r *http.Request) {
	store := s.journalStore(w)
	if store == nil {
		return
	}
	runID := r.PathValue("run")
	fromSeq, _ := strconv.ParseInt(r.URL.Query().Get("from_seq"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	} else if limit > 1000 {
		limit = 1000
	}
	// Over-read by one to know whether a next page actually exists.
	entries, err := store.Read(r.Context(), runID, fromSeq, limit+1)
	if errors.Is(err, journal.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "journal run not found")
		return
	}
	if errors.Is(err, journal.ErrUnknownFormat) {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	meta, err := store.ReadMeta(runID)
	if err != nil && !errors.Is(err, journal.ErrRunNotFound) {
		writeError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	if entries == nil {
		entries = []journal.Entry{}
	}
	var nextSeq int64
	if len(entries) > limit {
		entries = entries[:limit]
		nextSeq = entries[len(entries)-1].Ref.Seq + 1
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id":   runID,
		"meta":     meta,
		"entries":  entries,
		"next_seq": nextSeq, // 0 when this page reached the end
	})
}

// GET /v0/journal/{run}/export.md — PR-comment-ready markdown evidence.
func (s *Server) handleJournalExport(w http.ResponseWriter, r *http.Request) {
	store := s.journalStore(w)
	if store == nil {
		return
	}
	md, err := store.ExportMarkdown(r.Context(), r.PathValue("run"))
	if errors.Is(err, journal.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "journal run not found")
		return
	}
	if errors.Is(err, journal.ErrUnknownFormat) {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, md)
}

// GET /v0/journal/{run}/artifacts/{path...} — fetch one artifact.
func (s *Server) handleJournalArtifactGet(w http.ResponseWriter, r *http.Request) {
	store := s.journalStore(w)
	if store == nil {
		return
	}
	// Accept both the bare stored name and the run-relative form from
	// entry artifacts[].path (which already starts with "artifacts/").
	relPath := r.PathValue("path")
	if !strings.HasPrefix(relPath, "artifacts/") {
		relPath = "artifacts/" + relPath
	}
	rc, err := store.OpenArtifact(r.PathValue("run"), relPath)
	if errors.Is(err, journal.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "artifact not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	defer rc.Close()
	// The extension comes from the uploader-supplied name, so only serve
	// media types that are safe to render inline; everything else is
	// forced to a download to keep uploaded HTML/SVG from executing in
	// the daemon's origin.
	ctype := mime.TypeByExtension(filepath.Ext(relPath))
	switch {
	case strings.HasPrefix(ctype, "image/") && ctype != "image/svg+xml",
		strings.HasPrefix(ctype, "video/"),
		ctype == "text/plain; charset=utf-8":
	default:
		ctype = "application/octet-stream"
		w.Header().Set("Content-Disposition", "attachment")
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// POST /v0/journal/{run}/artifacts?name=&kind= — ingest an artifact (body =
// raw bytes) and journal an entry referencing it. Used by action backends
// and clients uploading evidence (e.g. screenshots).
func (s *Server) handleJournalArtifactPut(w http.ResponseWriter, r *http.Request) {
	store := s.journalStore(w)
	if store == nil {
		return
	}
	runID := r.PathValue("run")
	// Only accept artifacts for runs that already exist (i.e. a lease that
	// journaled at least one event); otherwise any caller could materialize
	// arbitrary run directories on disk.
	if _, err := store.ReadMeta(runID); errors.Is(err, journal.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "journal run not found")
		return
	}
	// The run's lease must still be active: finished runs are immutable
	// evidence. (The v0 protocol has no caller identity beyond the lease
	// ID itself; agent-scoped authz needs a protocol-wide authn slice.)
	var agentID string
	if s.leases != nil {
		l, err := s.leases.Get(runID)
		if err != nil || l.State != proto.LeaseActive {
			writeError(w, http.StatusConflict, proto.ErrLeaseExpired,
				"run's lease is not active; journal is immutable")
			return
		}
		agentID = l.AgentID
	}
	// Only artifact-bearing kinds are accepted so uploads can't forge
	// protocol entries (e.g. kind=lease) into the evidence log. Validated
	// before the body is stored so rejected uploads leave nothing on disk.
	// The query kinds map onto the journal/v0 entry-kind enumeration
	// (docs/journal.md): screenshot → observation, video → segment. The
	// requested kind is preserved in params as artifact_kind.
	artifactKind := r.URL.Query().Get("kind")
	var entryKind string
	switch artifactKind {
	case "":
		artifactKind = "observation"
		entryKind = "observation"
	case "observation", "screenshot":
		entryKind = "observation"
	case "video":
		entryKind = "segment"
	default:
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest,
			"kind must be one of observation, screenshot, video")
		return
	}
	name := r.URL.Query().Get("name")
	ref, err := store.PutArtifact(runID, name, http.MaxBytesReader(w, r.Body, maxArtifactBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	jref := s.journal.Record(r.Context(), journal.Event{
		Kind:      entryKind,
		LeaseID:   runID,
		AgentID:   agentID,
		Action:    "journal.artifact",
		Params:    map[string]any{"name": name, "artifact_kind": artifactKind},
		Status:    "ok",
		Artifacts: []journal.ArtifactRef{ref},
	})
	writeJSON(w, http.StatusCreated, map[string]any{"artifact": ref, "journal_ref": jref})
}
