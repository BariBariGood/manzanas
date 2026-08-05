package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/state"
	"github.com/BariBariGood/manzanas/proto"
)

// wsWriteTimeout bounds each outbound WS write.
const wsWriteTimeout = 10 * time.Second

// maxAsyncWSRequests bounds long-running requests (images.build /
// images.stamp, action dispatch) in flight per connection.
const maxAsyncWSRequests = 4

// asyncWSMethod reports whether a method may block for a long time (image
// builds hold the store mutex for minutes) and must therefore be
// dispatched off the connection's read loop so lease renewals and other
// requests keep flowing.
func asyncWSMethod(method string) bool {
	switch method {
	case proto.MethodImagesBuild, proto.MethodImagesStamp, proto.MethodImagesDelete:
		return true
	}
	return false
}

// actionWSMethod reports action-dispatch methods. They also run off the
// read loop (a wait_* can poll the a11y bridge for up to two minutes),
// but on a single per-connection worker so pipelined actions execute in
// the order they were sent.
func actionWSMethod(method string) bool {
	return method == proto.MethodAction || method == proto.MethodActionBatch
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // origin checks are out of scope for v0 (LAN/tailnet daemon)
	})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusInternalError, "server error")

	// connCtx is cancelled when the writer goroutine exits (e.g. write
	// error against a stalled peer), so anything blocked sending to the
	// events channel unblocks instead of hanging forever.
	connCtx, connCancel := context.WithCancel(r.Context())
	defer connCancel()
	ctx := connCtx
	events := s.events.subscribe()
	defer s.events.unsubscribe(events)
	// jout carries journal.tail acks and entries. It is separate from the
	// hub's events channel so a long backlog replay can't fill that buffer
	// and starve lease/target broadcasts (the hub drops when full).
	jout := make(chan proto.Envelope, 64)
	// tails tracks this connection's journal.tail subscriptions: one per
	// run, bounded, so repeated subscribes can't pile up watcher
	// goroutines for the connection's lifetime.
	tails := make(map[string]struct{})

	// Writer goroutine: serializes responses and events onto the socket.
	// Each write gets a bounded deadline so a stalled peer can't pin the
	// handler; on exit it cancels connCtx so senders to events unblock.
	write := func(env proto.Envelope) error {
		wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
		defer cancel()
		return wsjson.Write(wctx, conn, env)
	}
	out := make(chan proto.Envelope, 16)
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		defer connCancel()
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-events:
				if write(env) != nil {
					return
				}
			case env := <-jout:
				if write(env) != nil {
					return
				}
			case env, ok := <-out:
				if !ok {
					return
				}
				if write(env) != nil {
					return
				}
			}
		}
	}()

	// Methods that can block on the image store (a build holds its mutex
	// for minutes) are dispatched off the read loop so the connection
	// stays responsive — e.g. lease renewals must not queue behind a
	// build. Responses still serialize through the writer goroutine;
	// slots bounds in-flight work per connection. The WaitGroup keeps
	// `out` open until they finish.
	slots := make(chan struct{}, maxAsyncWSRequests)
	var pending sync.WaitGroup

	// Action envelopes run on one dedicated worker so pipelined actions on
	// this connection execute strictly in order while the read loop stays
	// free for lease renewals; the bounded queue rejects excess pipelining
	// as overloaded.
	actionQ := make(chan proto.Envelope, maxAsyncWSRequests)
	pending.Add(1)
	go func() {
		defer pending.Done()
		for env := range actionQ {
			resp := s.dispatchWS(ctx, env)
			select {
			case out <- resp:
			case <-writeDone:
			}
		}
	}()

readLoop:
	for {
		var env proto.Envelope
		if err := wsjson.Read(ctx, conn, &env); err != nil {
			break
		}
		if actionWSMethod(env.Method) {
			select {
			case actionQ <- env:
			default:
				select {
				case out <- wsError(env.ID, proto.ErrOverloaded, "too many in-flight requests on this connection"):
				case <-writeDone:
					break readLoop
				}
			}
			continue
		}
		if asyncWSMethod(env.Method) {
			select {
			case slots <- struct{}{}:
			default:
				select {
				case out <- wsError(env.ID, proto.ErrOverloaded, "too many in-flight requests on this connection"):
				case <-writeDone:
					break readLoop
				}
				continue
			}
			pending.Add(1)
			go func(env proto.Envelope) {
				defer pending.Done()
				defer func() { <-slots }()
				resp := s.dispatchWS(ctx, env)
				select {
				case out <- resp:
				case <-writeDone:
				}
			}(env)
			continue
		}
		var resp proto.Envelope
		if env.Method == proto.MethodJournalTail {
			resp = s.startJournalTail(ctx, env, jout, tails)
			if resp.ID == "" && resp.Error == nil {
				continue // ack already queued on the journal channel
			}
		} else {
			resp = s.dispatchWS(ctx, env)
		}
		select {
		case out <- resp:
		case <-writeDone:
			break readLoop
		}
	}
	close(actionQ)
	pending.Wait()
	close(out)
	<-writeDone
	conn.Close(websocket.StatusNormalClosure, "")
}

func wsResult(id string, v any) proto.Envelope {
	raw, err := json.Marshal(v)
	if err != nil {
		return wsError(id, proto.ErrInternal, err.Error())
	}
	return proto.Envelope{V: proto.Version, ID: id, Result: raw}
}

func wsError(id, code, msg string) proto.Envelope {
	return proto.Envelope{V: proto.Version, ID: id, Error: &proto.Error{Code: code, Message: msg}}
}

func (s *Server) dispatchWS(ctx context.Context, env proto.Envelope) proto.Envelope {
	switch env.Method {
	case proto.MethodListTargets:
		targets, err := s.listTargets(ctx)
		if err != nil {
			return wsError(env.ID, proto.ErrInternal, err.Error())
		}
		return wsResult(env.ID, map[string]any{"targets": targets})

	case proto.MethodAcquireLease:
		var req proto.AcquireLeaseRequest
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		if req.AgentID == "" {
			return wsError(env.ID, proto.ErrBadRequest, "agent_id is required")
		}
		if !state.ValidResetSpec(req.Reset) {
			return wsError(env.ID, proto.ErrBadRequest, "reset must be none, erase, or snapshot:<name>")
		}
		if !s.resetSpecSupported(req.Reset) {
			return wsError(env.ID, proto.ErrNotImplemented, "reset is not implemented in this build")
		}
		if req.Record != "" && req.Record != "video" {
			return wsError(env.ID, proto.ErrBadRequest, `record must be empty or "video"`)
		}
		if req.Record == "video" && s.recorder == nil {
			return wsError(env.ID, proto.ErrNotImplemented, "recording is not implemented in this build")
		}
		if s.deviceResetConflict(ctx, req) {
			return wsError(env.ID, proto.ErrBadRequest,
				"reset is not supported for physical devices; acquire the lease without a reset")
		}
		l, err := s.leases.Acquire(ctx, req)
		if errors.Is(err, lease.ErrNoMatch) {
			return wsError(env.ID, proto.ErrNoMatch, noMatchMessage(req))
		}
		if err != nil {
			return wsError(env.ID, proto.ErrInternal, err.Error())
		}
		if s.refuseDeviceResetGrant(ctx, l) {
			return wsError(env.ID, proto.ErrBadRequest,
				"reset is not supported for physical devices; acquire the lease without a reset")
		}
		s.syncRunOpen(l.ID)
		s.startRun(ctx, l)
		// Auto-record starts here only if the granted target is already
		// Booted; otherwise the boot path starts it.
		s.maybeAutoRecord(l)
		s.record(ctx, journal.Event{
			Kind: "lease", LeaseID: l.ID, AgentID: l.AgentID, Action: proto.MethodAcquireLease,
			Params: acquireParams(req),
			Status: "ok", Extra: map[string]any{"lease_state": string(l.State)},
		})
		return wsResult(env.ID, l)

	case proto.MethodGetLease:
		var req struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		l, err := s.leases.Get(req.ID)
		if err != nil {
			return wsError(env.ID, proto.ErrNotFound, "lease not found")
		}
		return wsResult(env.ID, l)

	case proto.MethodRenewLease:
		var req struct {
			ID         string `json:"id"`
			TTLSeconds int    `json:"ttl_seconds"`
		}
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		l, err := s.leases.Renew(req.ID, req.TTLSeconds)
		switch {
		case errors.Is(err, lease.ErrNotFound):
			return wsError(env.ID, proto.ErrNotFound, "lease not found")
		case errors.Is(err, lease.ErrNotActive):
			return wsError(env.ID, proto.ErrLeaseExpired, "lease is not active")
		case err != nil:
			return wsError(env.ID, proto.ErrInternal, err.Error())
		}
		s.record(ctx, journal.Event{
			Kind: "lease", LeaseID: l.ID, AgentID: l.AgentID, Action: proto.MethodRenewLease,
			Params: map[string]any{"ttl_seconds": req.TTLSeconds}, Status: "ok",
		})
		return wsResult(env.ID, l)

	case proto.MethodReleaseLease:
		var req struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		prior, priorErr := s.leases.Get(req.ID)
		l, err := s.leases.Release(ctx, req.ID)
		if err != nil {
			return wsError(env.ID, proto.ErrNotFound, "lease not found")
		}
		defer s.syncRunOpen(l.ID)
		// Auto-stop the lease's recording off the request path (SIGINT +
		// reap can take seconds; release must not block on mp4 finalize).
		if priorErr == nil && prior.State == proto.LeaseActive {
			go s.stopRecordingForLease(prior, "lease_end")
		}
		// Release is a no-op on already-terminal leases; only journal actual
		// transitions so a finished run's record stays immutable.
		if priorErr == nil && prior.State != proto.LeaseReleased && prior.State != proto.LeaseExpired {
			s.record(ctx, journal.Event{
				Kind: "lease", LeaseID: l.ID, AgentID: l.AgentID, Action: proto.MethodReleaseLease,
				Status: "ok",
			})
			if s.onLeaseEnd != nil && l.TargetUDID != "" {
				s.onLeaseEnd(l.TargetUDID)
			}
		}
		return wsResult(env.ID, l)

	case proto.MethodBootTarget, proto.MethodShutdown:
		return s.wsTargetOp(ctx, env)

	case proto.MethodJournalTail:
		// Handled in handleWS (needs the connection's event channel).
		return wsError(env.ID, proto.ErrInternal, "journal.tail must be handled by the connection")

	case proto.MethodStateSnapshot, proto.MethodStateRestore, proto.MethodStateErase,
		proto.MethodStateFixture, proto.MethodStateSnapshotsList, proto.MethodStateSnapshotsDelete:
		return s.dispatchStateWS(ctx, env)

	case proto.MethodImagesBuild, proto.MethodImagesList,
		proto.MethodImagesStamp, proto.MethodImagesDelete:
		return s.dispatchImagesWS(ctx, env)

	case proto.MethodStreamOpen:
		if s.streamer == nil {
			return wsError(env.ID, proto.ErrNotImplemented, env.Method+" is not implemented in this build")
		}
		var req proto.StreamRequest
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		offer, code, err := s.openStream(ctx, req)
		if err != nil {
			return wsError(env.ID, code, err.Error())
		}
		return wsResult(env.ID, offer)

	case proto.MethodAction:
		var req proto.ActionRequest
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		res, wireErr := s.dispatchAction(ctx, req)
		if wireErr != nil {
			return wsError(env.ID, wireErr.Code, wireErr.Message)
		}
		return wsResult(env.ID, res)

	case proto.MethodActionBatch:
		var req proto.BatchActionRequest
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		res, wireErr := s.dispatchActionBatch(ctx, req)
		if wireErr != nil {
			return wsError(env.ID, wireErr.Code, wireErr.Message)
		}
		return wsResult(env.ID, res)

	default:
		return wsError(env.ID, proto.ErrBadRequest, "unknown method: "+env.Method)
	}
}

func (s *Server) wsTargetOp(ctx context.Context, env proto.Envelope) proto.Envelope {
	var req struct {
		UDID    string `json:"udid"`
		LeaseID string `json:"lease_id"`
	}
	if err := json.Unmarshal(env.Params, &req); err != nil {
		return wsError(env.ID, proto.ErrBadRequest, err.Error())
	}
	l, err := s.leases.Get(req.LeaseID)
	if err != nil {
		return wsError(env.ID, proto.ErrNotFound, "lease not found")
	}
	if l.State != proto.LeaseActive {
		return wsError(env.ID, proto.ErrLeaseExpired, "lease is not active")
	}
	if l.TargetUDID != req.UDID {
		return wsError(env.ID, proto.ErrBadRequest, "lease does not hold this target")
	}
	op := s.bootTarget
	if env.Method == proto.MethodShutdown {
		op = s.shutdownTarget
	}
	record := func(status, errMsg string) {
		s.record(ctx, journal.Event{
			Kind: "action", LeaseID: req.LeaseID, AgentID: l.AgentID, Action: env.Method,
			Params: map[string]any{"udid": req.UDID}, Status: status, Error: errMsg,
		})
	}
	if err := op(ctx, l, req.UDID); err != nil {
		record("error", err.Error())
		_, code := targetOpErr(err)
		return wsError(env.ID, code, err.Error())
	}
	record("ok", "")
	t, err := s.reg.Get(ctx, req.UDID)
	if err != nil {
		return wsError(env.ID, proto.ErrInternal, err.Error())
	}
	s.events.broadcast(proto.EventTargetState, t)
	return wsResult(env.ID, t)
}

// startJournalTail subscribes the connection to live journal entries for a
// run: existing entries from from_seq are replayed first, then new entries
// stream as "journal.entry" events on the connection's journal channel. The
// subscription ack is sent through the same channel, ahead of any entry, so
// clients always see the ack before replayed entries. Errors are returned
// as the response envelope; on success the returned envelope is zero (the
// ack is already queued) and the caller must not enqueue it.
// maxTailsPerConn bounds journal.tail subscriptions per connection.
const maxTailsPerConn = 16

func (s *Server) startJournalTail(ctx context.Context, env proto.Envelope, events chan proto.Envelope, tails map[string]struct{}) proto.Envelope {
	store := s.journal.Store()
	if store == nil {
		return wsError(env.ID, proto.ErrNotImplemented, "journal is not implemented in this build")
	}
	var req struct {
		RunID   string `json:"run_id"`
		FromSeq int64  `json:"from_seq"`
	}
	if err := json.Unmarshal(env.Params, &req); err != nil {
		return wsError(env.ID, proto.ErrBadRequest, err.Error())
	}
	if req.RunID == "" {
		return wsError(env.ID, proto.ErrBadRequest, "run_id is required")
	}
	if _, ok := tails[req.RunID]; ok {
		return wsError(env.ID, proto.ErrBadRequest, "already tailing run "+req.RunID)
	}
	if len(tails) >= maxTailsPerConn {
		return wsError(env.ID, proto.ErrBadRequest, "too many journal.tail subscriptions on this connection")
	}
	live, cancel, err := store.Watch(req.RunID)
	if err != nil {
		return wsError(env.ID, proto.ErrBadRequest, err.Error())
	}
	// Subscribe before replay so no entry falls between replay and live.
	backlog, err := store.Read(ctx, req.RunID, req.FromSeq, 0)
	if err != nil && !errors.Is(err, journal.ErrRunNotFound) {
		cancel()
		if errors.Is(err, journal.ErrUnknownFormat) {
			return wsError(env.ID, proto.ErrNotImplemented, err.Error())
		}
		return wsError(env.ID, proto.ErrInternal, err.Error())
	}
	tails[req.RunID] = struct{}{}
	ack := wsResult(env.ID, map[string]any{"run_id": req.RunID, "from_seq": req.FromSeq})
	select {
	case events <- ack:
	case <-ctx.Done():
		cancel()
		return proto.Envelope{}
	}
	go func() {
		defer cancel()
		send := func(e journal.Entry) bool {
			raw, err := json.Marshal(e)
			if err != nil {
				return true
			}
			select {
			case events <- proto.Envelope{V: proto.Version, Event: proto.EventJournalEntry, Result: raw}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		// Seed with from_seq-1 so entries the client asked to skip are
		// filtered even when the replay backlog was empty.
		lastSeq := req.FromSeq - 1
		for _, e := range backlog {
			if !send(e) {
				return
			}
			lastSeq = e.Ref.Seq
		}
		// Periodic reconcile so entries dropped at the very end of a run
		// (with no later entry to reveal the gap) are still back-filled.
		// The tick is cheap: it only re-reads the log when LastSeq (an
		// in-memory counter) shows something past lastSeq.
		reconcile := time.NewTicker(5 * time.Second)
		defer reconcile.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-reconcile.C:
				last, err := store.LastSeq(req.RunID)
				if err != nil || last <= lastSeq {
					continue
				}
				missed, err := store.Read(ctx, req.RunID, lastSeq+1, 0)
				if err != nil {
					continue
				}
				for _, m := range missed {
					if m.Ref.Seq <= lastSeq {
						continue
					}
					if !send(m) {
						return
					}
					lastSeq = m.Ref.Seq
				}
				// LastSeq counts entries a Read can't return (oversized/
				// unreadable lines); adopt it once the back-fill is drained
				// so the next tick doesn't rescan the log forever.
				if last > lastSeq {
					lastSeq = last
				}
			case e := <-live:
				if e.Ref.Seq <= lastSeq {
					continue // already replayed
				}
				// A seq gap means the watcher buffer overflowed and entries
				// were dropped; back-fill the missing range from disk so the
				// tail stays a complete record.
				if e.Ref.Seq > lastSeq+1 {
					missed, err := store.Read(ctx, req.RunID, lastSeq+1, 0)
					if err == nil {
						for _, m := range missed {
							if m.Ref.Seq <= lastSeq || m.Ref.Seq >= e.Ref.Seq {
								continue
							}
							if !send(m) {
								return
							}
							lastSeq = m.Ref.Seq
						}
					}
				}
				if !send(e) {
					return
				}
				lastSeq = e.Ref.Seq
			}
		}
	}()
	return proto.Envelope{}
}
