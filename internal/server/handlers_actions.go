package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions"
	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/proto"
)

// actionStatus maps an actions.Error code onto an HTTP status.
func actionStatus(code string) int {
	switch code {
	case proto.ErrBadRequest:
		return http.StatusBadRequest
	case proto.ErrNotFound:
		return http.StatusNotFound
	case proto.ErrUnavailable:
		return http.StatusServiceUnavailable
	case proto.ErrTargetNotBooted:
		return http.StatusConflict
	case proto.ErrTimeout:
		return http.StatusRequestTimeout
	case proto.ErrNotImplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

// dispatchAction validates the lease and runs the action, returning the
// result or a wire error. Shared by the HTTP and WS surfaces.
func (s *Server) dispatchAction(ctx context.Context, req proto.ActionRequest) (proto.ActionResult, *proto.Error) {
	if s.actions == nil {
		return proto.ActionResult{}, &proto.Error{Code: proto.ErrNotImplemented,
			Message: "actions is not implemented in this build"}
	}
	if req.Kind == "" {
		return proto.ActionResult{}, &proto.Error{Code: proto.ErrBadRequest, Message: "kind is required"}
	}
	l, err := s.leases.Get(req.LeaseID)
	if err != nil {
		return proto.ActionResult{}, &proto.Error{Code: proto.ErrNotFound, Message: "lease not found"}
	}
	if l.State != proto.LeaseActive {
		return proto.ActionResult{}, &proto.Error{Code: proto.ErrLeaseExpired, Message: "lease is not active"}
	}
	if l.TargetUDID == "" {
		return proto.ActionResult{}, &proto.Error{Code: proto.ErrBadRequest, Message: "lease holds no target"}
	}
	res, err := s.actions.Dispatch(ctx, l.TargetUDID, req)
	ev := journal.Event{
		Kind: "action", LeaseID: l.ID, AgentID: l.AgentID, Action: req.Kind,
		Params: req.Payload, Status: "ok",
	}
	if err != nil {
		ev.Status, ev.Error = "error", err.Error()
		s.journal.Record(ctx, ev)
		return proto.ActionResult{}, actions.WireError(err)
	}
	// Lift the backend's a11y evidence into the journal entry: explicit
	// before/after hashes (opt-in around HID actions), or the tree hash an
	// observe/wait computed, which describes the tree at/after the action.
	if h, _ := res.Result["ax_before"].(string); h != "" {
		ev.AXBefore = h
	}
	if h, _ := res.Result["ax_after"].(string); h != "" {
		ev.AXAfter = h
	} else if h, _ := res.Result["hash"].(string); h != "" {
		ev.AXAfter = h
	}
	if req.Kind == "screenshot" {
		s.journalScreenshot(res.Result, req.Payload, &ev)
	}
	if ref := s.journal.Record(ctx, ev); ref.RunID != "" && res.JournalRef == nil {
		res.JournalRef = &ref
	}
	return res, nil
}

// journalScreenshot stores a successful screenshot's image as a run artifact
// (so callers using inline:false can still fetch the capture from the
// journal when journaling is enabled) and strips the inline payload when
// the caller opted out. inline:false is always honored on the wire — the
// caller explicitly declined the bytes; on a journal-less daemon that
// matches the pre-artifact contract where the pixels were never encoded.
// Best-effort: a store failure leaves the entry without an artifact ref.
func (s *Server) journalScreenshot(result, payload map[string]any, ev *journal.Event) {
	// Whitelist the backend-supplied format before using it as a result
	// key and artifact extension (defense in depth; the eval runner clamps
	// the same way).
	format, _ := result["format"].(string)
	if format != "jpeg" {
		format = "png"
	}
	key := format + "_base64"
	b64, _ := result[key].(string)
	if b64 == "" {
		return
	}
	if store := s.journal.Store(); store != nil {
		if img, err := base64.StdEncoding.DecodeString(b64); err == nil {
			if ref, err := store.PutArtifact(ev.LeaseID, "screenshot."+format, bytes.NewReader(img)); err == nil {
				ev.Artifacts = append(ev.Artifacts, ref)
			}
		}
	}
	if inline, ok := payload["inline"].(bool); ok && !inline {
		delete(result, key)
	}
}

// maxBatchActions and batchBudget bound one batch request so a single
// call can't hold an action pipeline for unbounded time: entries past
// the wall-clock budget are reported as timeouts instead of running
// (individual wait_* actions alone can otherwise stack to an hour).
const (
	maxBatchActions = 32
	batchBudget     = 5 * time.Minute
)

// dispatchActionBatch runs an ordered action list against the batch's
// lease. Each action revalidates the lease (it may expire mid-batch) and
// is journaled individually, exactly as if dispatched alone. Shared by
// the HTTP and WS surfaces.
func (s *Server) dispatchActionBatch(ctx context.Context, req proto.BatchActionRequest) (proto.BatchActionResult, *proto.Error) {
	if s.actions == nil {
		return proto.BatchActionResult{}, &proto.Error{Code: proto.ErrNotImplemented,
			Message: "actions is not implemented in this build"}
	}
	if len(req.Actions) == 0 {
		return proto.BatchActionResult{}, &proto.Error{Code: proto.ErrBadRequest, Message: "actions must be a non-empty array"}
	}
	if len(req.Actions) > maxBatchActions {
		return proto.BatchActionResult{}, &proto.Error{Code: proto.ErrBadRequest,
			Message: fmt.Sprintf("too many actions in one batch (max %d)", maxBatchActions)}
	}
	ctx, cancel := context.WithTimeout(ctx, batchBudget)
	defer cancel()
	out := proto.BatchActionResult{OK: true, Results: make([]proto.BatchItemResult, 0, len(req.Actions))}
	dispatched := 0
	for _, a := range req.Actions {
		if ctx.Err() != nil {
			out.OK = false
			out.Results = append(out.Results, proto.BatchItemResult{Error: &proto.Error{
				Code: proto.ErrTimeout, Message: fmt.Sprintf("batch budget (%s) exhausted; remaining actions not run", batchBudget),
			}})
			break
		}
		res, wireErr := s.dispatchAction(ctx, proto.ActionRequest{
			LeaseID: req.LeaseID, Kind: a.Kind, Payload: a.Payload,
		})
		dispatched++
		if wireErr != nil {
			out.OK = false
			out.Results = append(out.Results, proto.BatchItemResult{Error: wireErr})
			if req.StopOnError {
				break
			}
			continue
		}
		out.Results = append(out.Results, proto.BatchItemResult{
			OK: true, Result: res.Result, JournalRef: res.JournalRef,
		})
	}
	out.Completed = dispatched
	return out, nil
}

func (s *Server) handleActionBatch(w http.ResponseWriter, r *http.Request) {
	var req proto.BatchActionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, wireErr := s.dispatchActionBatch(r.Context(), req)
	if wireErr != nil {
		status := actionStatus(wireErr.Code)
		if wireErr.Code == proto.ErrNotImplemented {
			status = http.StatusNotImplemented
		}
		writeError(w, status, wireErr.Code, wireErr.Message)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	var req proto.ActionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, wireErr := s.dispatchAction(r.Context(), req)
	if wireErr != nil {
		status := actionStatus(wireErr.Code)
		switch wireErr.Code {
		case proto.ErrNotImplemented:
			status = http.StatusNotImplemented
		case proto.ErrLeaseExpired:
			status = http.StatusGone
		}
		writeError(w, status, wireErr.Code, wireErr.Message)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
