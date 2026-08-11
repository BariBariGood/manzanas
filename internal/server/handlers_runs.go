package server

import (
	"net/http"

	"github.com/BariBariGood/manzanas/proto"
)

// POST /v0/runs — the one-call run primitive: acquire lease -> boot ->
// fixtures -> install -> launch -> steps -> artifacts -> release, from a
// single declarative spec. Sync by default (the response is the finished
// run); async:true answers 202 immediately with the pending run for
// polling via GET /v0/runs/{id}. See proto/PROTOCOL.md §9 and docs/runs.md.
func (s *Server) handleRunCreate(w http.ResponseWriter, r *http.Request) {
	var req proto.RunRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.NormalizeAgentID()
	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, "agent_id is required (session_id is accepted as an alias)")
		return
	}
	if s.leases == nil {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented, "leases are not available in this build")
		return
	}
	if wireErr := s.validateRunSpec(req.Spec); wireErr != nil {
		status := http.StatusBadRequest
		if wireErr.Code == proto.ErrNotImplemented {
			status = http.StatusNotImplemented
		}
		writeWireError(w, status, wireErr)
		return
	}
	h := s.runs.create(req.Spec, req.AgentID)
	if h == nil {
		writeWireError(w, http.StatusServiceUnavailable, &proto.Error{
			Code:              proto.ErrOverloaded,
			Message:           "too many runs in flight; retry when one finishes",
			RetryAfterSeconds: int(overloadRetryAfter.Seconds()),
		})
		return
	}
	go s.executeRun(h)
	if req.Async {
		writeJSON(w, http.StatusAccepted, h.snapshot())
		return
	}
	// Sync: the run's own budget (timeouts.run_seconds, capped) bounds
	// execution, so waiting on done is bounded too. If the client goes
	// away the run keeps executing — the deferred release is
	// unconditional either way.
	select {
	case <-h.done:
		writeJSON(w, http.StatusOK, h.snapshot())
	case <-r.Context().Done():
		// Client gone; nothing to write.
	}
}

// GET /v0/runs — retained runs, newest first.
func (s *Server) handleRunList(w http.ResponseWriter, r *http.Request) {
	runs := s.runs.list()
	// The export can be large; keep listings lean.
	for i := range runs {
		runs[i].ExportMD = ""
	}
	writeJSON(w, http.StatusOK, proto.RunList{Runs: runs})
}

// GET /v0/runs/{id} — one run resource (export_md included once finished,
// when the spec asked for it).
func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request) {
	h, ok := s.runs.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "run not found")
		return
	}
	writeJSON(w, http.StatusOK, h.snapshot())
}
