package server

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/state"
	"github.com/BariBariGood/manzanas/proto"
)

// noMatchMessage words a no_match refusal for what the request actually
// asked: a pinned UDID (unknown, or failing the labels) reads differently
// from a label set with no matching target.
func noMatchMessage(req proto.AcquireLeaseRequest) string {
	switch {
	case req.UDID != "" && len(req.Labels) > 0:
		return "pinned target " + req.UDID + " is unknown or does not match the requested labels"
	case req.UDID != "":
		return "pinned target " + req.UDID + " is unknown"
	default:
		return "no target matches the requested labels"
	}
}

func (s *Server) handleAcquireLease(w http.ResponseWriter, r *http.Request) {
	var req proto.AcquireLeaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, "agent_id is required")
		return
	}
	if !state.ValidResetSpec(req.Reset) {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, "reset must be none, erase, or snapshot:<name>")
		return
	}
	if !s.resetSpecSupported(req.Reset) {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented, "reset is not implemented in this build")
		return
	}
	if req.Record != "" && req.Record != "video" {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, `record must be empty or "video"`)
		return
	}
	if req.Record == "video" && s.recorder == nil {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented, "recording is not implemented in this build")
		return
	}
	if s.deviceResetConflict(r.Context(), req) {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest,
			"reset is not supported for physical devices; acquire the lease without a reset")
		return
	}
	l, err := s.leases.Acquire(r.Context(), req)
	if err != nil {
		if errors.Is(err, lease.ErrNoMatch) {
			writeError(w, http.StatusConflict, proto.ErrNoMatch, noMatchMessage(req))
			return
		}
		writeError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	if s.refuseDeviceResetGrant(r.Context(), l) {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest,
			"reset is not supported for physical devices; acquire the lease without a reset")
		return
	}
	status := http.StatusCreated
	if l.State == proto.LeaseQueued {
		status = http.StatusAccepted
	}
	s.syncRunOpen(l.ID)
	s.startRun(r.Context(), l)
	// Auto-record starts here only if the granted target is already Booted
	// (e.g. thawed from the warm pool); otherwise the boot handler starts it.
	s.maybeAutoRecord(l)
	s.record(r.Context(), journal.Event{
		Kind: "lease", LeaseID: l.ID, AgentID: l.AgentID, Action: proto.MethodAcquireLease,
		Params: acquireParams(req),
		Status: "ok", Extra: map[string]any{"lease_state": string(l.State)},
	})
	writeJSON(w, status, l)
}

// acquireParams builds the journal params for a lease acquisition.
func acquireParams(req proto.AcquireLeaseRequest) map[string]any {
	p := map[string]any{"labels": req.Labels, "ttl_seconds": req.TTLSeconds}
	if req.UDID != "" {
		p["udid"] = req.UDID
	}
	if req.Record != "" {
		p["record"] = req.Record
	}
	return p
}

func (s *Server) handleListLeases(w http.ResponseWriter, r *http.Request) {
	// Lease IDs are served as-is: v0 has no auth (tailnet-only), and the
	// journal endpoints already expose every lease ID as a run ID, so a
	// redacted listing bought no secrecy while breaking correlation with
	// GET /v0/leases/{id} and journal runs.
	writeJSON(w, http.StatusOK, map[string]any{"leases": s.leases.List()})
}

// deviceResetConflict reports whether an acquire request asks for a
// post-lease reset on a physical device: there is no `simctl erase`
// equivalent for devices, so such leases are refused up front instead of
// quarantining the device at release time.
func (s *Server) deviceResetConflict(ctx context.Context, req proto.AcquireLeaseRequest) bool {
	if req.Reset == "" || req.Reset == "none" {
		return false
	}
	if slices.Contains(req.Labels, string(proto.TargetDevice)) {
		return true
	}
	// Only daemons that can enumerate devices pay for the registry
	// lookups below; a simulator-only fleet can never match one.
	if !s.devicesEnabled {
		return false
	}
	if req.UDID != "" {
		if t, err := s.reg.Get(ctx, req.UDID); err == nil && t.Kind == proto.TargetDevice {
			return true
		}
	}
	// If every target the label set can match is a physical device, the
	// lease could only ever land (or queue) on one: refuse now rather
	// than letting it queue forever behind targets it must never take.
	if len(req.Labels) > 0 {
		if targets, err := s.reg.List(ctx); err == nil {
			anyMatch, allDevices := false, true
			for _, t := range targets {
				if !hasAllLabels(t.Labels, req.Labels) {
					continue
				}
				anyMatch = true
				if t.Kind != proto.TargetDevice {
					allDevices = false
				}
			}
			if anyMatch && allDevices {
				return true
			}
		}
	}
	return false
}

// hasAllLabels reports whether have contains every label in want.
func hasAllLabels(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// refuseDeviceResetGrant catches reset-carrying leases that landed on a
// physical device anyway (a label set can match a device without naming
// it, which the pre-grant check cannot see): the lease is released and
// the acquire refused, rather than silently skipping the reset later.
func (s *Server) refuseDeviceResetGrant(ctx context.Context, l proto.Lease) bool {
	if !s.devicesEnabled || l.Reset == "" || l.Reset == "none" || l.TargetUDID == "" {
		return false
	}
	t, err := s.reg.Get(ctx, l.TargetUDID)
	if err != nil || t.Kind != proto.TargetDevice {
		return false
	}
	// The reset never ran (and never will): drop it before releasing so
	// the release does not engage the reset machinery, which would
	// transiently sentinel the device and journal a skipped reset for an
	// acquire the caller was told failed.
	s.leases.DropReset(l.ID)
	if _, err := s.leases.Release(ctx, l.ID); err != nil {
		s.log.Warn("failed to release refused device lease", "lease", l.ID, "err", err)
	}
	return true
}

// resetSpecSupported reports whether a lease reset spec can actually run
// in this build: non-trivial specs need a wired state engine (otherwise
// the lease would silently never be reset).
func (s *Server) resetSpecSupported(reset string) bool {
	return s.state != nil || reset == "" || reset == "none"
}

func (s *Server) handleGetLease(w http.ResponseWriter, r *http.Request) {
	l, err := s.leases.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "lease not found")
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) handleRenewLease(w http.ResponseWriter, r *http.Request) {
	var req proto.RenewLeaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	l, err := s.leases.Renew(r.PathValue("id"), req.TTLSeconds)
	switch {
	case errors.Is(err, lease.ErrNotFound):
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "lease not found")
	case errors.Is(err, lease.ErrNotActive):
		writeError(w, http.StatusGone, proto.ErrLeaseExpired, "lease is not active")
	case err != nil:
		writeError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
	default:
		s.record(r.Context(), journal.Event{
			Kind: "lease", LeaseID: l.ID, AgentID: l.AgentID, Action: proto.MethodRenewLease,
			Params: map[string]any{"ttl_seconds": req.TTLSeconds}, Status: "ok",
		})
		writeJSON(w, http.StatusOK, l)
	}
}

func (s *Server) handleReleaseLease(w http.ResponseWriter, r *http.Request) {
	prior, priorErr := s.leases.Get(r.PathValue("id"))
	l, err := s.leases.Release(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "lease not found")
		return
	}
	defer s.syncRunOpen(l.ID)
	// Auto-stop the lease's recording off the request path (SIGINT + reap
	// can take seconds; release must not block on mp4 finalize). If the
	// lease requested a reset, ResetSink stops it first — whichever runs
	// first wins, the other is a no-op.
	if priorErr == nil && prior.State == proto.LeaseActive {
		go s.stopRecordingForLease(prior, "lease_end")
	}
	// Release is a no-op on already-terminal leases; only journal actual
	// transitions so a finished run's record stays immutable.
	if priorErr == nil && prior.State != proto.LeaseReleased && prior.State != proto.LeaseExpired {
		s.record(r.Context(), journal.Event{
			Kind: "lease", LeaseID: l.ID, AgentID: l.AgentID, Action: proto.MethodReleaseLease,
			Status: "ok",
		})
		if s.onLeaseEnd != nil && l.TargetUDID != "" {
			s.onLeaseEnd(l.TargetUDID)
		}
	}
	writeJSON(w, http.StatusOK, l)
}
