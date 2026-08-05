package server

import (
	"net/http"

	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/proto"
)

// SetDashReadonly disables the dashboard's mutating control endpoints
// (/v0/dash/...): they answer 403 read_only. The rest of the v0 API is
// unaffected — the same port always serves the full mutating protocol,
// so this is an operator foot-gun guard for shared viewing, not a
// security boundary.
func (s *Server) SetDashReadonly(on bool) { s.dashReadonly = on }

// handleDashConfig tells the dashboard UI whether its mutating controls
// are enabled so it can hide them instead of surfacing 403s.
func (s *Server) handleDashConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"readonly": s.dashReadonly})
}

// dashGate refuses dash mutations in --dash-readonly mode.
func (s *Server) dashGate(w http.ResponseWriter) bool {
	if s.dashReadonly {
		writeError(w, http.StatusForbidden, proto.ErrReadOnly,
			"dashboard controls are disabled (--dash-readonly)")
		return false
	}
	return true
}

// dashReserveTarget atomically holds a free target for a dash lifecycle
// op via the lease manager's reserve sentinel — the same primitive the
// warm pool uses — so the target cannot be granted to a lease (or wiped)
// mid-op. It refuses (with 409) targets held by an active lease (the
// holder owns its lifecycle), mid reset/rebuild, quarantined after a
// failed reset (pool recovery owns it), or parked in the warm pool (a
// lease thaws it). On success the caller must Unreserve.
func (s *Server) dashReserveTarget(w http.ResponseWriter, udid string) bool {
	if _, held := s.leases.Active(udid); held {
		writeError(w, http.StatusConflict, proto.ErrTargetBusy,
			"target is held by an active lease; release the lease first")
		return false
	}
	if s.parked != nil && s.parked(udid) {
		writeError(w, http.StatusConflict, proto.ErrTargetBusy,
			"target is parked in the warm pool; the pool owns its lifecycle")
		return false
	}
	takeover, ok := s.leases.Reserve(udid)
	if !ok {
		writeError(w, http.StatusConflict, proto.ErrTargetBusy,
			"target is being reset or rebuilt by the daemon; try again shortly")
		return false
	}
	if takeover {
		// Reserve took over a quarantined target; that hold belongs to
		// pool recovery, so put it back and refuse.
		s.leases.Quarantine(udid)
		writeError(w, http.StatusConflict, proto.ErrTargetBusy,
			"target is quarantined after a failed reset; pool recovery owns it")
		return false
	}
	return true
}

// handleDashTargetOp runs a leaseless boot/shutdown on a free target and
// answers like the leased op: 202 + the target's new state.
func (s *Server) handleDashTargetOp(w http.ResponseWriter, r *http.Request,
	op func(udid string) error) {
	if !s.dashGate(w) {
		return
	}
	udid := r.PathValue("udid")
	if !s.dashReserveTarget(w, udid) {
		return
	}
	defer s.leases.Unreserve(udid)
	if err := op(udid); err != nil {
		status, code := targetOpErr(err)
		writeError(w, status, code, err.Error())
		return
	}
	t, err := s.reg.Get(r.Context(), udid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	s.events.broadcast(proto.EventTargetState, t)
	writeJSON(w, http.StatusAccepted, t)
}

func (s *Server) handleDashBoot(w http.ResponseWriter, r *http.Request) {
	s.handleDashTargetOp(w, r, func(udid string) error {
		// No lease exists: the synthetic lease only feeds the recording
		// hooks (ClearPoisoned keys off TargetUDID; auto-record no-ops
		// because Record is empty).
		return s.bootTarget(r.Context(), proto.Lease{TargetUDID: udid}, udid)
	})
}

func (s *Server) handleDashShutdown(w http.ResponseWriter, r *http.Request) {
	s.handleDashTargetOp(w, r, func(udid string) error {
		return s.shutdownTarget(r.Context(), proto.Lease{TargetUDID: udid}, udid)
	})
}

// handleDashReleaseLease releases whatever active lease holds the target.
// The dashboard addresses leases by target and never handles lease IDs
// (capability tokens for the mutating endpoints), even though the fleet
// listing now serves them for correlation.
func (s *Server) handleDashReleaseLease(w http.ResponseWriter, r *http.Request) {
	if !s.dashGate(w) {
		return
	}
	udid := r.PathValue("udid")
	prior, held := s.leases.Active(udid)
	if !held {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "target has no active lease")
		return
	}
	l, err := s.leases.Release(r.Context(), prior.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "lease not found")
		return
	}
	defer s.syncRunOpen(l.ID)
	// Same post-release path as DELETE /v0/leases/{id}: stop the lease's
	// recording off the request path and journal the transition. A lease
	// that expired between the Active lookup and Release comes back
	// terminal without a transition — the expiry path owns its journal
	// entry and recording stop, so a finished run's record stays immutable.
	if l.State == proto.LeaseReleased {
		go s.stopRecordingForLease(prior, "lease_end")
		s.record(r.Context(), journal.Event{
			Kind: "lease", LeaseID: l.ID, AgentID: l.AgentID, Action: proto.MethodReleaseLease,
			Status: "ok", Extra: map[string]any{"released_by": "dash"},
		})
		if s.onLeaseEnd != nil && l.TargetUDID != "" {
			s.onLeaseEnd(l.TargetUDID)
		}
	}
	// The released lease is returned with its ID redacted: the dashboard
	// never sees capability tokens.
	l.ID = ""
	writeJSON(w, http.StatusOK, l)
}

// handleDashRecordingStop stops whatever recording is live on the target,
// regardless of the run that started it, and ingests the result.
func (s *Server) handleDashRecordingStop(w http.ResponseWriter, r *http.Request) {
	if s.recorder == nil {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented,
			"recording is not implemented in this build")
		return
	}
	if !s.dashGate(w) {
		return
	}
	udid := r.PathValue("udid")
	if !s.recorder.Recording(udid) {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "target has no live recording")
		return
	}
	res, first, err := s.recorder.Stop(udid, "dash_stop")
	if err != nil {
		writeError(w, http.StatusConflict, proto.ErrTargetBusy, err.Error())
		return
	}
	if !first {
		writeError(w, http.StatusConflict, proto.ErrTargetBusy,
			"recording was already being stopped")
		return
	}
	out := s.ingestRecording(r.Context(), "", res)
	if !out.OK {
		msg := "recording ingest failed"
		if res.Err != nil {
			msg = "recording failed validation: " + res.Err.Error()
		}
		writeError(w, http.StatusUnprocessableEntity, proto.ErrInternal, msg)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
