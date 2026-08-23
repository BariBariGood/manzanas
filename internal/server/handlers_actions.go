package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
	case proto.ErrOffViewport:
		return http.StatusConflict
	case proto.ErrAmbiguousMatch:
		return http.StatusConflict
	case proto.ErrFocusRequired:
		return http.StatusPreconditionFailed
	case proto.ErrAssertionFailed:
		return http.StatusExpectationFailed
	case proto.ErrForbidden:
		return http.StatusForbidden
	case proto.ErrLeasePaused:
		return http.StatusConflict
	case proto.ErrNotImplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

// pausedAllowedKinds are the read-only action kinds still dispatchable on
// a paused lease, so the holder can watch the human's progress without
// touching the device.
var pausedAllowedKinds = map[string]bool{
	"observe":    true,
	"screenshot": true,
}

// journalParams elides bulky inline image blobs from the journaled
// payload: the journal skips lines over its read cap, so an entry
// carrying a multi-megabyte template_base64 would vanish from reads,
// tails, and exports (and bloat retention) if stored verbatim.
func journalParams(p map[string]any) map[string]any {
	b64, ok := p["template_base64"].(string)
	if !ok {
		return p
	}
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = v
	}
	out["template_base64"] = journal.ElideBlob(len(b64))
	return out
}

// journalExtraKeys is the whitelist of action-result keys lifted
// verbatim into journal Event.Extra: additive provenance/verification
// fields other slices ride (journal/v0 readers ignore unknown keys).
var journalExtraKeys = []string{"resolver", "resolver_skipped", "input_mode", "verified", "verify", "fingerprint"}

// dispatchAction validates the lease and runs the action, returning the
// result or a wire error. Shared by the HTTP and WS surfaces.
func (s *Server) dispatchAction(ctx context.Context, req proto.ActionRequest) (proto.ActionResult, *proto.Error) {
	if s.actions == nil {
		return proto.ActionResult{}, &proto.Error{Code: proto.ErrNotImplemented,
			Message: "actions is not implemented in this build"}
	}
	if req.Kind == "" {
		return proto.ActionResult{}, &proto.Error{Code: proto.ErrBadRequest, Message: `kind is required (action requests are {"lease_id","kind","payload"}; see proto/PROTOCOL.md §5)`}
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
	if l.Paused && !pausedAllowedKinds[req.Kind] {
		msg := "lease is paused"
		if l.PausedReason != "" {
			msg += " (" + l.PausedReason + ")"
		}
		msg += ": only read-only observation is allowed until POST /v0/leases/{id}/resume"
		return proto.ActionResult{}, &proto.Error{Code: proto.ErrLeasePaused, Message: msg}
	}
	// The lease ID rides the context so backend policy scoped to one
	// lease (the resolver ladder's AX circuit breaker) can key on it.
	actx := actions.WithLeaseID(ctx, l.ID)
	if s.journal.Store() != nil {
		// A journal store can keep escalation screenshots, so the
		// resolver ladder is allowed to spend time capturing them.
		actx = actions.WithEvidenceSink(actx)
	}
	res, err := s.actions.Dispatch(actx, l.TargetUDID, req)
	ev := journal.Event{
		Kind: "action", LeaseID: l.ID, AgentID: l.AgentID, Action: req.Kind,
		Params: journalParams(req.Payload), Status: "ok",
	}
	if err != nil {
		wireErr := actions.WireError(err)
		if wireErr.Code == proto.ErrTargetNotBooted {
			// A leased target that is not booted is a lifecycle anomaly:
			// tell the holder who shut it down and why (or that the
			// daemon didn't), so it can journal/recover instead of
			// guessing. See PROTOCOL.md §1 (error detail).
			wireErr.Detail = s.targetDownDetail(l.TargetUDID)
			ev.Extra = map[string]any{"udid": l.TargetUDID, "detail": wireErr.Detail}
		}
		// A resolver-ladder escalation carries an observation bundle
		// (rung trail + last text boxes + screenshot): journal it as
		// evidence so the escalating agent has something to re-plan on.
		if bundle, png := actions.EscalationEvidence(err); bundle != nil {
			if ev.Extra == nil {
				ev.Extra = map[string]any{}
			}
			ev.Extra["escalation"] = bundle
			if store := s.journal.Store(); store != nil && len(png) > 0 {
				if ref, aerr := store.PutArtifact(ev.LeaseID, "escalation.png", bytes.NewReader(png)); aerr == nil {
					ev.Artifacts = append(ev.Artifacts, ref)
				}
			}
		}
		ev.Status, ev.Error = "error", err.Error()
		s.journal.Record(ctx, ev)
		return proto.ActionResult{}, wireErr
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
		s.journalImage(res.Result, req.Payload, &ev, "screenshot")
	}
	if req.Kind == "audit" {
		s.journalAudit(res.Result, req.Payload, &ev)
	}
	s.journalLogs(res.Result, &ev)
	// Lift whitelisted cross-slice result keys (resolution provenance,
	// verification, fingerprints) into the journal entry verbatim.
	for _, k := range journalExtraKeys {
		if v, ok := res.Result[k]; ok {
			if ev.Extra == nil {
				ev.Extra = map[string]any{}
			}
			ev.Extra[k] = v
		}
	}
	if ref := s.journal.Record(ctx, ev); ref.RunID != "" && res.JournalRef == nil {
		res.JournalRef = &ref
	}
	return res, nil
}

// journalImage stores a successful capture's image as a run artifact named
// base.<format> (so callers using inline:false can still fetch the capture
// from the journal when journaling is enabled) and strips the inline
// payload when the caller opted out. inline:false is always honored on the
// wire — the caller explicitly declined the bytes; on a journal-less daemon
// that matches the pre-artifact contract where the pixels were never
// encoded. Best-effort: a store failure leaves the entry without an
// artifact ref.
func (s *Server) journalImage(result, payload map[string]any, ev *journal.Event, base string) {
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
			if ref, err := store.PutArtifact(ev.LeaseID, base+"."+format, bytes.NewReader(img)); err == nil {
				ev.Artifacts = append(ev.Artifacts, ref)
			}
		}
	}
	if inline, ok := payload["inline"].(bool); ok && !inline {
		delete(result, key)
	}
}

// journalAudit stores an audit's two evidence outputs as run artifacts:
// the findings JSON and the annotated screenshot. Best-effort, like
// journalImage.
func (s *Server) journalAudit(result, payload map[string]any, ev *journal.Event) {
	if store := s.journal.Store(); store != nil {
		if f, ok := result["findings"]; ok {
			if b, err := json.MarshalIndent(f, "", "  "); err == nil {
				if ref, err := store.PutArtifact(ev.LeaseID, "audit-findings.json", bytes.NewReader(b)); err == nil {
					ev.Artifacts = append(ev.Artifacts, ref)
				}
			}
		}
	}
	s.journalImage(result, payload, ev, "audit-annotated")
}

// journalLogs stores an action's captured os_log lines (capture_logs:
// true) as a run artifact, correlating the log window with the journal
// step. Best-effort, like journalImage; the inline "logs" field stays on
// the wire either way — the caller asked for the lines.
func (s *Server) journalLogs(result map[string]any, ev *journal.Event) {
	logs, _ := result["logs"].(string)
	if logs == "" {
		return
	}
	if store := s.journal.Store(); store != nil {
		if ref, err := store.PutArtifact(ev.LeaseID, "action-logs.txt", bytes.NewReader([]byte(logs))); err == nil {
			ev.Artifacts = append(ev.Artifacts, ref)
		}
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
		writeWireError(w, status, wireErr)
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
		writeWireError(w, status, wireErr)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
