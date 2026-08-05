package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/internal/warm"
	"github.com/BariBariGood/manzanas/proto"
)

// listTargets lists registry targets with the warm pool's warm flag
// stamped on (nil-safe); shared by the REST and WS targets.list surfaces
// so both return identical bodies.
func (s *Server) listTargets(ctx context.Context) ([]proto.Target, error) {
	targets, err := s.reg.List(ctx)
	if err != nil {
		return nil, err
	}
	if targets == nil {
		targets = []proto.Target{}
	}
	s.markWarm(targets)
	s.markRecording(targets)
	return targets, nil
}

func (s *Server) handleListTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.listTargets(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": targets})
}

// markRecording stamps Target.Recording on targets with a live video
// recording (nil-safe when recording is not wired).
func (s *Server) markRecording(targets []proto.Target) {
	if s.recorder == nil {
		return
	}
	for i := range targets {
		targets[i].Recording = s.recorder.Recording(targets[i].UDID)
	}
}

// markWarm stamps Target.Warm on parked pool sims; both target-listing
// surfaces (REST and WS) use it so they stay identical.
func (s *Server) markWarm(targets []proto.Target) {
	if s.parked == nil {
		return
	}
	for i := range targets {
		targets[i].Warm = s.parked(targets[i].UDID)
	}
}

type targetOpRequest struct {
	LeaseID string `json:"lease_id"`
}

// requireLease validates that the given lease is active and holds udid,
// returning the lease on success.
func (s *Server) requireLease(w http.ResponseWriter, leaseID, udid string) (proto.Lease, bool) {
	l, err := s.leases.Get(leaseID)
	if err != nil {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "lease not found")
		return proto.Lease{}, false
	}
	if l.State != proto.LeaseActive {
		writeError(w, http.StatusGone, proto.ErrLeaseExpired, "lease is not active")
		return proto.Lease{}, false
	}
	if l.TargetUDID != udid {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, "lease does not hold this target")
		return proto.Lease{}, false
	}
	return l, true
}

// targetOpErr maps a registry/pool operation failure to an HTTP status
// and protocol error code. The warm pool's gate refusals (load, disk,
// running cap) are deliberate, transient throttles: clients must see
// ErrOverloaded (back off and retry), not an internal fault.
func targetOpErr(err error) (int, string) {
	var nf *registry.NotFoundError
	var db *registry.DeviceBootError
	var ds *registry.DeviceShutdownError
	switch {
	case errors.As(err, &nf):
		return http.StatusNotFound, proto.ErrNotFound
	case errors.As(err, &db), errors.As(err, &ds):
		// A deliberate refusal (physical devices cannot be booted or
		// shut down remotely), not a daemon fault.
		return http.StatusNotImplemented, proto.ErrNotImplemented
	case errors.Is(err, warm.ErrLoadTooHigh),
		errors.Is(err, warm.ErrDiskTooLow),
		errors.Is(err, warm.ErrTooManyRunning):
		return http.StatusServiceUnavailable, proto.ErrOverloaded
	default:
		return http.StatusInternalServerError, proto.ErrInternal
	}
}

func (s *Server) handleTargetOp(w http.ResponseWriter, r *http.Request,
	action string, op func(l proto.Lease) error) {
	udid := r.PathValue("udid")
	var req targetOpRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	l, ok := s.requireLease(w, req.LeaseID, udid)
	if !ok {
		return
	}
	record := func(status, errMsg string) {
		s.record(r.Context(), journal.Event{
			Kind: "action", LeaseID: req.LeaseID, AgentID: l.AgentID, Action: action,
			Params: map[string]any{"udid": udid}, Status: status, Error: errMsg,
		})
	}
	if err := op(l); err != nil {
		record("error", err.Error())
		status, code := targetOpErr(err)
		writeError(w, status, code, err.Error())
		return
	}
	record("ok", "")
	t, err := s.reg.Get(r.Context(), udid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	s.events.broadcast(proto.EventTargetState, t)
	writeJSON(w, http.StatusAccepted, t)
}

func (s *Server) handleBoot(w http.ResponseWriter, r *http.Request) {
	s.handleTargetOp(w, r, proto.MethodBootTarget,
		func(l proto.Lease) error { return s.bootTarget(r.Context(), l, r.PathValue("udid")) })
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	s.handleTargetOp(w, r, proto.MethodShutdown,
		func(l proto.Lease) error { return s.shutdownTarget(r.Context(), l, r.PathValue("udid")) })
}

// bootTarget is the recording-aware boot shared by the HTTP and WS
// surfaces.
func (s *Server) bootTarget(ctx context.Context, l proto.Lease, udid string) error {
	wasBooted := false
	if t, err := s.reg.Get(ctx, udid); err == nil {
		wasBooted = t.State == proto.StateBooted
	}
	if err := s.reg.Boot(ctx, udid); err != nil {
		return err
	}
	// A shutdown+boot cycle is the documented recovery for a poisoned
	// recording session; a no-op boot of an already-Booted sim is not.
	if s.recorder != nil && !wasBooted {
		s.recorder.ClearPoisoned(l.TargetUDID)
	}
	s.maybeAutoRecord(l)
	return nil
}

// shutdownTarget is the recording-aware shutdown shared by the HTTP and
// WS surfaces.
func (s *Server) shutdownTarget(ctx context.Context, l proto.Lease, udid string) error {
	// Drain any recording — including a previous run's still-finalizing
	// one — before the sim goes away: shutting a sim down mid-recording
	// hangs the recordVideo child indefinitely.
	s.drainRecording(udid, "target_shutdown")
	if err := s.reg.Shutdown(ctx, udid); err != nil {
		return err
	}
	// Shutting the sim down is what actually clears a wedged host
	// recording session.
	if s.recorder != nil {
		s.recorder.ClearPoisoned(l.TargetUDID)
	}
	return nil
}
