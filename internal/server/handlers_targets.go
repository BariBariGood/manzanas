package server

import (
	"context"
	"errors"
	"net/http"
	"time"

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

// overloadRetryAfter is the retry hint attached to overload 503s
// (Retry-After header + retry_after_seconds in the body): gate refusals
// are load-driven and clear on this order of timescale, so callers must
// not busy-poll.
const overloadRetryAfter = 30 * time.Second

// isOverloadErr reports whether err is one of the warm pool's transient
// boot-gate refusals.
func isOverloadErr(err error) bool {
	return errors.Is(err, warm.ErrLoadTooHigh) ||
		errors.Is(err, warm.ErrDiskTooLow) ||
		errors.Is(err, warm.ErrTooManyRunning)
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
		if errors.Is(err, errWaitLeaseGone) {
			writeError(w, http.StatusGone, proto.ErrLeaseExpired, err.Error())
			return
		}
		if errors.Is(err, errWaitBusy) {
			writeWireError(w, http.StatusServiceUnavailable, &proto.Error{
				Code: proto.ErrOverloaded, Message: err.Error(),
				RetryAfterSeconds: int(overloadRetryAfter.Seconds()),
			})
			return
		}
		status, code := targetOpErr(err)
		if code == proto.ErrOverloaded {
			// Cooperative throttle: tell the caller when to come back
			// instead of leaving it to busy-poll (issue: agents hammered
			// the gate for 15+ minutes). See PROTOCOL.md §2.
			writeWireError(w, status, &proto.Error{
				Code: code, Message: err.Error(),
				RetryAfterSeconds: int(overloadRetryAfter.Seconds()),
			})
			return
		}
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

// Boot-wait (opt-in via POST /v0/targets/{udid}/boot?wait=true): instead
// of answering 503 on a gate refusal, the daemon retries the boot
// server-side until it is accepted or the budget runs out, so callers
// need not busy-poll. bootWaitPoll is overridable in tests.
const (
	defaultBootWaitPoll = 5 * time.Second
	bootWaitBudget      = 10 * time.Minute
	// maxBootWaiters caps concurrent server-side boot waits so a flood of
	// ?wait=true requests can't pin goroutines/connections against an
	// already-overloaded host; excess requests get the hinted 503.
	maxBootWaiters = 16
)

// errWaitLeaseGone aborts a boot wait whose lease ended (expired,
// released, or regranted elsewhere) while waiting for capacity: booting
// a target the caller no longer holds could wake a sim mid-reset or one
// now owned by another holder.
var errWaitLeaseGone = errors.New("lease is no longer active on this target; boot wait aborted")

// errWaitBusy refuses a second concurrent ?wait=true boot for the same
// lease: one server-side wait per lease, so a single holder can't pin
// every waiter slot (and its connections) with repeated wait requests.
var errWaitBusy = errors.New("a boot wait is already in flight for this lease")

func (s *Server) handleBoot(w http.ResponseWriter, r *http.Request) {
	udid := r.PathValue("udid")
	wait := r.URL.Query().Get("wait") == "true" || r.URL.Query().Get("wait") == "1"
	s.handleTargetOp(w, r, proto.MethodBootTarget, func(l proto.Lease) error {
		if wait {
			return s.bootTargetWait(r.Context(), l, udid)
		}
		return s.bootTarget(r.Context(), l, udid)
	})
}

// bootTargetWait retries a gate-refused boot until it is accepted, the
// request context ends, or the wait budget runs out (then the last
// overload error is returned and mapped to a 503 with a retry hint).
func (s *Server) bootTargetWait(ctx context.Context, l proto.Lease, udid string) error {
	s.waitMu.Lock()
	if s.waitByLease[l.ID] {
		s.waitMu.Unlock()
		return errWaitBusy
	}
	s.waitByLease[l.ID] = true
	s.waitMu.Unlock()
	defer func() {
		s.waitMu.Lock()
		delete(s.waitByLease, l.ID)
		s.waitMu.Unlock()
	}()
	select {
	case s.bootWaitSlots <- struct{}{}:
		defer func() { <-s.bootWaitSlots }()
	default:
		// Waiter cap reached: fall back to a single gated attempt so the
		// caller gets the usual hinted 503 instead of queueing further.
		return s.bootTarget(ctx, l, udid)
	}
	poll := s.bootWaitPoll
	if poll <= 0 {
		poll = defaultBootWaitPoll
	}
	ctx, cancel := context.WithTimeout(ctx, bootWaitBudget)
	defer cancel()
	var err error
	first := true
	for {
		// Re-validate the lease on every attempt: the wait can outlive
		// the lease (TTL + grace < budget), and a boot after that could
		// wake a sim mid post-lease reset or one granted to someone else.
		if cur, gerr := s.leases.Get(l.ID); gerr != nil ||
			cur.State != proto.LeaseActive || cur.TargetUDID != udid {
			return errWaitLeaseGone
		}
		// After the first refusal, probe the cheap gates before another
		// full boot attempt: a full attempt lists every target, and doing
		// that per poll per waiter amplifies load on an already-overloaded
		// host.
		if first || s.bootGates == nil {
			err = s.bootTarget(ctx, l, udid)
		} else if err = s.bootGates(udid); err == nil {
			err = s.bootTarget(ctx, l, udid)
		}
		first = false
		if err == nil || !isOverloadErr(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(poll):
		}
	}
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
	// A successful boot invalidates the shutdown ledger entry: a later
	// not-booted error must not blame a shutdown that this boot undid.
	s.clearShutdownNote(udid)
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
	actor, reason := "agent "+l.AgentID, "requested shutdown under lease "+redactLeaseID(l.ID)
	if l.ID == "" {
		// The dashboard's leaseless shutdown path passes a bare lease.
		actor, reason = "dashboard", "operator-requested shutdown"
	}
	s.NoteShutdown(udid, actor, reason)
	// Shutting the sim down is what actually clears a wedged host
	// recording session.
	if s.recorder != nil {
		s.recorder.ClearPoisoned(l.TargetUDID)
	}
	return nil
}
