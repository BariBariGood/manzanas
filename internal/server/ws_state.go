package server

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/BariBariGood/manzanas/internal/state"
	"github.com/BariBariGood/manzanas/proto"
)

// wsStateError maps state-engine errors to WS error envelopes.
func wsStateError(id string, err error) proto.Envelope {
	switch {
	case errors.Is(err, state.ErrTargetBusy):
		return wsError(id, proto.ErrTargetBusy, err.Error())
	case errors.Is(err, state.ErrSnapshotNotFound):
		return wsError(id, proto.ErrNotFound, err.Error())
	case errors.Is(err, state.ErrBadFixture), errors.Is(err, state.ErrBadReset):
		return wsError(id, proto.ErrBadRequest, err.Error())
	default:
		return wsError(id, proto.ErrInternal, err.Error())
	}
}

// wsLeasedTarget resolves an active lease's target UDID for WS state ops.
func (s *Server) wsLeasedTarget(ctx context.Context, leaseID string) (string, *proto.Envelope) {
	l, err := s.leases.Get(leaseID)
	if err != nil {
		e := wsError("", proto.ErrNotFound, "lease not found")
		return "", &e
	}
	if l.State != proto.LeaseActive {
		e := wsError("", proto.ErrLeaseExpired, "lease is not active")
		return "", &e
	}
	if s.stateOpOnDevice(ctx, l.TargetUDID) {
		e := wsError("", proto.ErrNotImplemented,
			"state operations are simulator-only; leased target is a physical device")
		return "", &e
	}
	return l.TargetUDID, nil
}

// wsSnapshotOwnedBy reports whether snapshot id exists and was taken from
// the leased target udid (mirrors snapshotOwnedBy on the REST side). A
// failure to read the snapshot index is returned as an error rather than
// being conflated with "not found".
func (s *Server) wsSnapshotOwnedBy(ctx context.Context, udid, id string) (bool, error) {
	snaps, err := s.state.ListSnapshots(ctx)
	if err != nil {
		return false, err
	}
	for _, sn := range snaps {
		if sn.ID == id && sn.SourceUDID == udid {
			return true, nil
		}
	}
	return false, nil
}

// dispatchStateWS handles the state.* WS methods (mirroring the REST
// handlers in handlers_state.go).
func (s *Server) dispatchStateWS(ctx context.Context, env proto.Envelope) proto.Envelope {
	if s.state == nil {
		return wsError(env.ID, proto.ErrNotImplemented, "state is not implemented in this build")
	}
	switch env.Method {
	case proto.MethodStateSnapshot:
		var req proto.SnapshotRequest
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		udid, fail := s.wsLeasedTarget(ctx, req.LeaseID)
		if fail != nil {
			fail.ID = env.ID
			return *fail
		}
		info, err := s.state.Snapshot(ctx, udid, req.Label)
		s.recordState(ctx, req.LeaseID, proto.MethodStateSnapshot,
			map[string]any{"udid": udid, "label": req.Label}, err)
		if err != nil {
			return wsStateError(env.ID, err)
		}
		return wsResult(env.ID, info)

	case proto.MethodStateRestore:
		var req proto.RestoreRequest
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		if req.Snapshot == "" {
			return wsError(env.ID, proto.ErrBadRequest, "snapshot is required")
		}
		udid, fail := s.wsLeasedTarget(ctx, req.LeaseID)
		if fail != nil {
			fail.ID = env.ID
			return *fail
		}
		// A restore shuts a booted sim down; a recordVideo child hangs
		// if its sim goes away mid-recording, so drain any recording
		// first.
		s.drainRecording(udid, "target_shutdown")
		rebooted, err := s.state.Restore(ctx, udid, req.Snapshot, req.Reboot)
		s.recordState(ctx, req.LeaseID, proto.MethodStateRestore,
			map[string]any{"udid": udid, "snapshot": req.Snapshot, "reboot": req.Reboot}, err)
		if err != nil {
			return wsStateError(env.ID, err)
		}
		return wsResult(env.ID, proto.RestoreResult{OK: true, Snapshot: req.Snapshot, Rebooted: rebooted})

	case proto.MethodStateErase:
		var req proto.EraseRequest
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		udid, fail := s.wsLeasedTarget(ctx, req.LeaseID)
		if fail != nil {
			fail.ID = env.ID
			return *fail
		}
		err := s.state.Erase(ctx, udid)
		s.recordState(ctx, req.LeaseID, proto.MethodStateErase,
			map[string]any{"udid": udid}, err)
		if err != nil {
			return wsStateError(env.ID, err)
		}
		return wsResult(env.ID, map[string]any{"ok": true})

	case proto.MethodStateFixture:
		var req proto.FixtureRequest
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		if req.Name == "" {
			return wsError(env.ID, proto.ErrBadRequest, "name is required")
		}
		udid, fail := s.wsLeasedTarget(ctx, req.LeaseID)
		if fail != nil {
			fail.ID = env.ID
			return *fail
		}
		err := s.state.ApplyFixture(ctx, udid, req.Name, req.Payload)
		s.recordState(ctx, req.LeaseID, proto.MethodStateFixture,
			map[string]any{"udid": udid, "name": req.Name}, err)
		if err != nil {
			return wsStateError(env.ID, err)
		}
		return wsResult(env.ID, map[string]any{"ok": true})

	case proto.MethodStateSnapshotsList:
		var req struct {
			LeaseID string `json:"lease_id"`
		}
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		udid, fail := s.wsLeasedTarget(ctx, req.LeaseID)
		if fail != nil {
			fail.ID = env.ID
			return *fail
		}
		snaps, err := s.state.ListSnapshots(ctx)
		if err != nil {
			return wsStateError(env.ID, err)
		}
		return wsResult(env.ID, map[string]any{"snapshots": filterBySource(snaps, udid)})

	case proto.MethodStateSnapshotsDelete:
		var req struct {
			LeaseID string `json:"lease_id"`
			ID      string `json:"id"`
		}
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		udid, fail := s.wsLeasedTarget(ctx, req.LeaseID)
		if fail != nil {
			fail.ID = env.ID
			return *fail
		}
		owned, err := s.wsSnapshotOwnedBy(ctx, udid, req.ID)
		if err != nil {
			return wsStateError(env.ID, err)
		}
		if !owned {
			return wsError(env.ID, proto.ErrNotFound, "snapshot not found for leased target")
		}
		err = s.state.DeleteSnapshot(ctx, req.ID)
		s.recordState(ctx, req.LeaseID, proto.MethodStateSnapshotsDelete,
			map[string]any{"udid": udid, "id": req.ID}, err)
		if err != nil {
			return wsStateError(env.ID, err)
		}
		return wsResult(env.ID, map[string]any{"ok": true})

	default:
		return wsError(env.ID, proto.ErrBadRequest, "unknown method: "+env.Method)
	}
}
