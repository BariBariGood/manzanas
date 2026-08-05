package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/state"
	"github.com/BariBariGood/manzanas/proto"
)

// recordState journals one mutating state-engine op (kind "state") under
// the lease it ran against; shared by the REST and WS surfaces.
func (s *Server) recordState(ctx context.Context, leaseID, action string, params map[string]any, opErr error) {
	var agentID string
	if l, err := s.leases.Get(leaseID); err == nil {
		agentID = l.AgentID
	}
	status, msg := "ok", ""
	if opErr != nil {
		status, msg = "error", opErr.Error()
	}
	s.record(ctx, journal.Event{
		Kind: "state", LeaseID: leaseID, AgentID: agentID, Action: action,
		Params: params, Status: status, Error: msg,
	})
}

// leasedTarget validates that leaseID is an active lease and returns its
// target UDID; on failure it writes the error response and returns "".
// Every mutating state op goes through this guard, so the engine only ever
// touches targets covered by the requesting lease.
func (s *Server) leasedTarget(ctx context.Context, w http.ResponseWriter, leaseID string) string {
	if leaseID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, "lease_id is required")
		return ""
	}
	l, err := s.leases.Get(leaseID)
	if err != nil {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "lease not found")
		return ""
	}
	if l.State != proto.LeaseActive {
		writeError(w, http.StatusGone, proto.ErrLeaseExpired, "lease is not active")
		return ""
	}
	if s.stateOpOnDevice(ctx, l.TargetUDID) {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented,
			"state operations are simulator-only; leased target is a physical device")
		return ""
	}
	return l.TargetUDID
}

// stateOpOnDevice reports whether udid is a confirmed physical device, so
// the simulator-only state engine refuses it up front. Only consulted
// when devices are enabled; an indeterminate lookup falls through to the
// engine, which fails loudly on its own.
func (s *Server) stateOpOnDevice(ctx context.Context, udid string) bool {
	if !s.devicesEnabled {
		return false
	}
	t, err := s.reg.Get(ctx, udid)
	return err == nil && t.Kind == proto.TargetDevice
}

// writeStateError maps state-engine errors to wire errors.
func writeStateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, state.ErrTargetBusy):
		writeError(w, http.StatusConflict, proto.ErrTargetBusy, err.Error())
	case errors.Is(err, state.ErrSnapshotNotFound):
		writeError(w, http.StatusNotFound, proto.ErrNotFound, err.Error())
	case errors.Is(err, state.ErrBadFixture), errors.Is(err, state.ErrBadReset):
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
	}
}

func (s *Server) handleStateSnapshot(w http.ResponseWriter, r *http.Request) {
	var req proto.SnapshotRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	udid := s.leasedTarget(r.Context(), w, req.LeaseID)
	if udid == "" {
		return
	}
	info, err := s.state.Snapshot(r.Context(), udid, req.Label)
	s.recordState(r.Context(), req.LeaseID, proto.MethodStateSnapshot,
		map[string]any{"udid": udid, "label": req.Label}, err)
	if err != nil {
		writeStateError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) handleStateRestore(w http.ResponseWriter, r *http.Request) {
	var req proto.RestoreRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Snapshot == "" {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, "snapshot is required")
		return
	}
	udid := s.leasedTarget(r.Context(), w, req.LeaseID)
	if udid == "" {
		return
	}
	// A restore shuts a booted sim down; a recordVideo child hangs if
	// its sim goes away mid-recording, so drain any recording first.
	s.drainRecording(udid, "target_shutdown")
	rebooted, err := s.state.Restore(r.Context(), udid, req.Snapshot, req.Reboot)
	s.recordState(r.Context(), req.LeaseID, proto.MethodStateRestore,
		map[string]any{"udid": udid, "snapshot": req.Snapshot, "reboot": req.Reboot}, err)
	if err != nil {
		writeStateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proto.RestoreResult{OK: true, Snapshot: req.Snapshot, Rebooted: rebooted})
}

func (s *Server) handleStateErase(w http.ResponseWriter, r *http.Request) {
	var req proto.EraseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	udid := s.leasedTarget(r.Context(), w, req.LeaseID)
	if udid == "" {
		return
	}
	err := s.state.Erase(r.Context(), udid)
	s.recordState(r.Context(), req.LeaseID, proto.MethodStateErase,
		map[string]any{"udid": udid}, err)
	if err != nil {
		writeStateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStateFixture(w http.ResponseWriter, r *http.Request) {
	var req proto.FixtureRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, "name is required")
		return
	}
	udid := s.leasedTarget(r.Context(), w, req.LeaseID)
	if udid == "" {
		return
	}
	err := s.state.ApplyFixture(r.Context(), udid, req.Name, req.Payload)
	s.recordState(r.Context(), req.LeaseID, proto.MethodStateFixture,
		map[string]any{"udid": udid, "name": req.Name}, err)
	if err != nil {
		writeStateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStateSnapshotsList(w http.ResponseWriter, r *http.Request) {
	udid := s.leasedTarget(r.Context(), w, r.URL.Query().Get("lease_id"))
	if udid == "" {
		return
	}
	snaps, err := s.state.ListSnapshots(r.Context())
	if err != nil {
		writeStateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": filterBySource(snaps, udid)})
}

// handleClearQuarantine is the operator escape hatch for a target
// quarantined by a failed auto-reset: it frees the target (no-op if it is
// not actually quarantined) and lets queued leases be granted again. It
// refuses (target_busy) while the reset is still running.
func (s *Server) handleClearQuarantine(w http.ResponseWriter, r *http.Request) {
	udid := r.PathValue("udid")
	cleared, err := s.leases.ClearQuarantine(udid)
	if err != nil {
		writeError(w, http.StatusConflict, proto.ErrTargetBusy, err.Error())
		return
	}
	if cleared {
		s.log.Warn("operator cleared reset quarantine", "target", udid)
	} else {
		s.log.Info("clear-quarantine no-op: target was not quarantined", "target", udid)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// filterBySource keeps only snapshots taken from udid, so a client never
// sees other targets' snapshot metadata.
func filterBySource(snaps []proto.SnapshotInfo, udid string) []proto.SnapshotInfo {
	out := []proto.SnapshotInfo{}
	for _, sn := range snaps {
		if sn.SourceUDID == udid {
			out = append(out, sn)
		}
	}
	return out
}

// snapshotOwnedBy verifies snapshot id exists and was taken from the leased
// target udid; on failure it writes not_found (existence of other targets'
// snapshots is not disclosed) and returns false.
func (s *Server) snapshotOwnedBy(w http.ResponseWriter, r *http.Request, udid, id string) bool {
	snaps, err := s.state.ListSnapshots(r.Context())
	if err != nil {
		writeStateError(w, err)
		return false
	}
	for _, sn := range snaps {
		if sn.ID == id && sn.SourceUDID == udid {
			return true
		}
	}
	writeError(w, http.StatusNotFound, proto.ErrNotFound, "snapshot not found for leased target")
	return false
}

func (s *Server) handleStateSnapshotsDelete(w http.ResponseWriter, r *http.Request) {
	udid := s.leasedTarget(r.Context(), w, r.URL.Query().Get("lease_id"))
	if udid == "" {
		return
	}
	id := r.PathValue("id")
	if !s.snapshotOwnedBy(w, r, udid, id) {
		return
	}
	err := s.state.DeleteSnapshot(r.Context(), id)
	s.recordState(r.Context(), r.URL.Query().Get("lease_id"), proto.MethodStateSnapshotsDelete,
		map[string]any{"udid": udid, "id": id}, err)
	if err != nil {
		writeStateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
