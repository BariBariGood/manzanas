package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions"
	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/internal/state"
	"github.com/BariBariGood/manzanas/proto"
)

// The run engine executes one RunSpec end-to-end: acquire lease -> boot ->
// fixtures -> install -> launch -> steps -> final artifacts -> release.
// Every stage goes through the same code paths the individual endpoints
// use (dispatchAction, bootTargetWait, the state engine), so a run's
// journal is indistinguishable from the same loop done call-by-call.

// Run stage names (Run.Stage).
const (
	runStageAcquire   = "acquire"
	runStageBoot      = "boot"
	runStageFixtures  = "fixtures"
	runStageInstall   = "install"
	runStageLaunch    = "launch"
	runStageSteps     = "steps"
	runStageArtifacts = "artifacts"
	runStageRelease   = "release"
)

// Run timeout defaults/caps (seconds).
const (
	defaultRunAcquireSeconds = 60
	defaultRunSeconds        = 600
	defaultRunStepSeconds    = 60
	// runLeaseTTLMargin pads the lease TTL past the run budget so the
	// lease never expires mid-run (release applies the reset either way).
	runLeaseTTLMargin = 120
	// maxRunSeconds keeps budget + margin within the lease manager's
	// MaxTTL so clampTTL never eats the margin.
	maxRunSeconds = int(lease.MaxTTL/time.Second) - runLeaseTTLMargin
	// runAcquirePoll is how often a queued run re-checks its lease.
	runAcquirePoll = 250 * time.Millisecond
)

// validateRunSpec rejects specs the daemon cannot execute, before any
// lease is taken. Returns a wire error or nil.
func (s *Server) validateRunSpec(spec proto.RunSpec) *proto.Error {
	if spec.Target.Image != "" {
		return &proto.Error{Code: proto.ErrNotImplemented,
			Message: "target.image (golden-image requirement) is reserved and not yet implemented; stamp sims from the image first (POST /v0/images/{id}/stamp) and match them by labels/udid"}
	}
	if len(spec.Target.Labels) == 0 && spec.Target.UDID == "" &&
		spec.Target.Runtime == "" && spec.Target.DeviceType == "" {
		return &proto.Error{Code: proto.ErrBadRequest,
			Message: "target requires at least one of labels, udid, runtime, or device_type"}
	}
	if spec.Target.Runtime != "" && len(registry.RequirementLabels(spec.Target.Runtime, "")) == 0 {
		return &proto.Error{Code: proto.ErrBadRequest,
			Message: fmt.Sprintf("target.runtime %q does not translate to a matchable label", spec.Target.Runtime)}
	}
	if spec.Target.DeviceType != "" && len(registry.RequirementLabels("", spec.Target.DeviceType)) == 0 {
		return &proto.Error{Code: proto.ErrBadRequest,
			Message: fmt.Sprintf("target.device_type %q does not translate to a matchable label", spec.Target.DeviceType)}
	}
	if !state.ValidResetSpec(spec.Target.Reset) {
		return &proto.Error{Code: proto.ErrBadRequest,
			Message: "target.reset must be none, erase, or snapshot:<name>"}
	}
	if !s.resetSpecSupported(spec.Target.Reset) {
		return &proto.Error{Code: proto.ErrNotImplemented,
			Message: "target.reset needs the state engine, which is not available in this build"}
	}
	if len(spec.Target.Fixtures) > 0 && s.state == nil {
		return &proto.Error{Code: proto.ErrNotImplemented,
			Message: "target.fixtures need the state engine, which is not available in this build"}
	}
	for _, f := range spec.Target.Fixtures {
		if f.Name == "" {
			return &proto.Error{Code: proto.ErrBadRequest, Message: "every fixture requires a name"}
		}
	}
	if app := spec.App; app != nil {
		launch := app.Launch == nil || *app.Launch
		if app.Path == "" && app.BundleID == "" {
			return &proto.Error{Code: proto.ErrBadRequest,
				Message: "app requires path (install) and/or bundle_id (launch)"}
		}
		if launch && app.BundleID == "" && app.Launch != nil {
			return &proto.Error{Code: proto.ErrBadRequest,
				Message: "app.launch=true requires app.bundle_id"}
		}
	}
	if s.actions == nil && (len(spec.Steps) > 0 || spec.App != nil) {
		return &proto.Error{Code: proto.ErrNotImplemented,
			Message: "actions is not implemented in this build; a run may not declare steps or an app"}
	}
	for i, st := range spec.Steps {
		if st.MaestroFlow != "" {
			return &proto.Error{Code: proto.ErrNotImplemented,
				Message: fmt.Sprintf("steps[%d]: maestro_flow is reserved and not yet implemented (see docs/runs.md); use native steps", i)}
		}
		if st.Action == "" {
			return &proto.Error{Code: proto.ErrBadRequest,
				Message: fmt.Sprintf("steps[%d]: action is required", i)}
		}
		if st.TimeoutSeconds < 0 {
			return &proto.Error{Code: proto.ErrBadRequest,
				Message: fmt.Sprintf("steps[%d]: timeout_seconds must be >= 0", i)}
		}
	}
	if spec.Timeouts.AcquireSeconds < 0 || spec.Timeouts.RunSeconds < 0 || spec.Timeouts.StepSeconds < 0 {
		return &proto.Error{Code: proto.ErrBadRequest, Message: "timeouts must be >= 0"}
	}
	return nil
}

// runTimeouts resolves a spec's timeouts to concrete values.
func runTimeouts(spec proto.RunSpec) (acquire, run, step time.Duration) {
	a := spec.Timeouts.AcquireSeconds
	if a == 0 {
		a = defaultRunAcquireSeconds
	}
	r := spec.Timeouts.RunSeconds
	if r == 0 {
		r = defaultRunSeconds
	}
	if r > maxRunSeconds {
		r = maxRunSeconds
	}
	st := spec.Timeouts.StepSeconds
	if st == 0 {
		st = defaultRunStepSeconds
	}
	return time.Duration(a) * time.Second, time.Duration(r) * time.Second, time.Duration(st) * time.Second
}

// runTargetLabels merges the spec's explicit labels with the slugs derived
// from runtime/device_type requirements.
func runTargetLabels(t proto.RunTarget) []string {
	labels := append([]string(nil), t.Labels...)
	for _, l := range registry.RequirementLabels(t.Runtime, t.DeviceType) {
		if !contains(labels, l) {
			labels = append(labels, l)
		}
	}
	return labels
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// executeRun runs the whole loop for h. It must be called on its own
// goroutine; the handle is finished (terminal state, slot released) on
// return no matter what failed.
func (s *Server) executeRun(h *runHandle) {
	defer s.runs.finish(h)
	// This goroutine is not under net/http's per-handler panic recovery;
	// contain a panicking backend to this run instead of the whole daemon.
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("run panicked", "run", h.snapshot().ID, "panic", p)
			s.finishRun(h, &proto.Error{Code: proto.ErrInternal,
				Message: fmt.Sprintf("run panicked: %v", p)})
		}
	}()
	spec := h.snapshot().Spec
	acquireTO, runTO, stepTO := runTimeouts(spec)
	ctx, cancel := context.WithTimeout(context.Background(), runTO)
	defer cancel()

	now := time.Now().UTC()
	h.update(func(r *proto.Run) {
		r.State = proto.RunRunning
		r.StartedAt = &now
		r.Stage = runStageAcquire
	})

	l, wireErr := s.runAcquire(ctx, h, acquireTO)
	if wireErr != nil {
		// A lease may have been created (queued then abandoned, or a
		// refused grant); its journal run still holds the evidence trail.
		if id := h.snapshot().LeaseID; id != "" {
			expCtx, expCancel := context.WithTimeout(context.Background(), 30*time.Second)
			s.attachRunExport(expCtx, h, id)
			expCancel()
		}
		s.finishRun(h, wireErr)
		return
	}
	// From here on the lease must always be released (with its reset),
	// whatever else fails; the deferred call is the safety net for
	// panics — the normal path releases explicitly below so the terminal
	// state is only published once the lease is free and the evidence
	// export is attached.
	released := false
	defer func() {
		if !released {
			s.runRelease(h, l.ID, false)
		}
	}()
	s.recordRunEvent(ctx, h, l, "runs.start", nil)

	stagesErr := s.runStages(ctx, h, l, stepTO)
	s.recordRunEvent(ctx, h, l, "runs.finish", stagesErr)
	released = true
	s.runRelease(h, l.ID, stagesErr != nil)
	s.finishRun(h, stagesErr)
}

// bootSettleTimeout bounds how long the boot stage waits for the target
// to report Booted after the boot was accepted.
const bootSettleTimeout = 3 * time.Minute

// bootSettlePoll paces the Booted-state polls of the boot stage.
const bootSettlePoll = 2 * time.Second

// waitTargetBooted polls the registry until the target reports Booted.
func (s *Server) waitTargetBooted(ctx context.Context, udid string) error {
	ctx, cancel := context.WithTimeout(ctx, bootSettleTimeout)
	defer cancel()
	for {
		st, err := s.reg.Health(ctx, udid)
		if err == nil && st == proto.StateBooted {
			return nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("target did not reach Booted: %w", err)
			}
			return fmt.Errorf("target did not reach Booted (still %s)", st)
		case <-time.After(bootSettlePoll):
		}
	}
}

// runStages executes every post-acquire stage; the first failure aborts.
func (s *Server) runStages(ctx context.Context, h *runHandle, l proto.Lease, stepTO time.Duration) *proto.Error {
	spec := h.snapshot().Spec
	h.update(func(r *proto.Run) { r.Stage = runStageBoot })
	// Physical devices cannot be booted (or polled for Booted): the boot
	// stage only applies to simulators, like the MCP acquire helper.
	if !s.stateOpOnDevice(ctx, l.TargetUDID) {
		if err := s.bootTargetWait(ctx, l, l.TargetUDID); err != nil {
			s.record(ctx, journal.Event{Kind: "action", LeaseID: l.ID, AgentID: l.AgentID,
				Action: proto.MethodBootTarget, Params: map[string]any{"udid": l.TargetUDID},
				Status: "error", Error: err.Error()})
			return runStageError(runStageBoot, err)
		}
		// An accepted boot is asynchronous (`simctl boot` returns while
		// the sim is still Booting); the install/launch stages need the
		// target actually up, so the boot stage only completes on Booted.
		if err := s.waitTargetBooted(ctx, l.TargetUDID); err != nil {
			s.record(ctx, journal.Event{Kind: "action", LeaseID: l.ID, AgentID: l.AgentID,
				Action: proto.MethodBootTarget, Params: map[string]any{"udid": l.TargetUDID},
				Status: "error", Error: err.Error()})
			return runStageError(runStageBoot, err)
		}
		s.record(ctx, journal.Event{Kind: "action", LeaseID: l.ID, AgentID: l.AgentID,
			Action: proto.MethodBootTarget, Params: map[string]any{"udid": l.TargetUDID}, Status: "ok"})
		if t, err := s.reg.Get(ctx, l.TargetUDID); err == nil {
			s.events.broadcast(proto.EventTargetState, t)
		}
	}

	if len(spec.Target.Fixtures) > 0 {
		h.update(func(r *proto.Run) { r.Stage = runStageFixtures })
		if s.stateOpOnDevice(ctx, l.TargetUDID) {
			return &proto.Error{Code: proto.ErrNotImplemented,
				Message: runStageFixtures + ": state operations are simulator-only; leased target is a physical device"}
		}
		for _, f := range spec.Target.Fixtures {
			err := s.state.ApplyFixture(ctx, l.TargetUDID, f.Name, f.Payload)
			s.recordState(ctx, l.ID, proto.MethodStateFixture,
				map[string]any{"udid": l.TargetUDID, "name": f.Name}, err)
			if err != nil {
				return runStateError(runStageFixtures, fmt.Errorf("fixture %q: %w", f.Name, err))
			}
		}
	}

	if app := spec.App; app != nil {
		if app.Path != "" {
			h.update(func(r *proto.Run) { r.Stage = runStageInstall })
			if _, wireErr := s.dispatchAction(ctx, proto.ActionRequest{
				LeaseID: l.ID, Kind: "install_app", Payload: map[string]any{"path": app.Path},
			}); wireErr != nil {
				return prefixWireError(runStageInstall, wireErr)
			}
		}
		if app.BundleID != "" && (app.Launch == nil || *app.Launch) {
			h.update(func(r *proto.Run) { r.Stage = runStageLaunch })
			payload := map[string]any{"bundle_id": app.BundleID}
			if app.TerminateRunning {
				payload["terminate_running"] = true
			}
			if len(app.Args) > 0 {
				args := make([]any, len(app.Args))
				for i, a := range app.Args {
					args[i] = a
				}
				payload["args"] = args
			}
			if _, wireErr := s.dispatchAction(ctx, proto.ActionRequest{
				LeaseID: l.ID, Kind: "launch_app", Payload: payload,
			}); wireErr != nil {
				return prefixWireError(runStageLaunch, wireErr)
			}
		}
	}

	h.update(func(r *proto.Run) { r.Stage = runStageSteps })
	// firstErr is the run's verdict (any step failure fails the run);
	// halted stops later steps only when the failing step did not opt
	// into continue_on_error.
	var firstErr *proto.Error
	halted := false
	for i, st := range spec.Steps {
		res := proto.RunStepResult{Index: i, Name: st.Name, Action: st.Action}
		if halted && !st.ContinueOnError {
			// A previous failure already doomed the run and this step did
			// not opt into running regardless.
			res.Status = proto.StepSkipped
			h.update(func(r *proto.Run) { r.Steps = append(r.Steps, res) })
			continue
		}
		to := stepTO
		if st.TimeoutSeconds > 0 {
			to = time.Duration(st.TimeoutSeconds) * time.Second
		}
		stepCtx, cancel := context.WithTimeout(ctx, to)
		started := time.Now()
		var actRes proto.ActionResult
		var wireErr *proto.Error
		if st.Action == "batch" {
			wireErr = s.runBatchStep(stepCtx, l.ID, st.With)
		} else {
			actRes, wireErr = s.dispatchAction(stepCtx, proto.ActionRequest{
				LeaseID: l.ID, Kind: st.Action, Payload: st.With,
			})
		}
		cancel()
		res.DurationMS = time.Since(started).Milliseconds()
		if wireErr != nil {
			res.Status = proto.StepError
			res.Error = wireErr
			if firstErr == nil {
				firstErr = prefixWireError(fmt.Sprintf("steps[%d] %s", i, st.Action), wireErr)
			}
			if !st.ContinueOnError {
				halted = true
			}
		} else {
			res.Status = proto.StepOK
			res.JournalRef = actRes.JournalRef
		}
		h.update(func(r *proto.Run) { r.Steps = append(r.Steps, res) })
	}

	if fs := spec.Artifacts.FinalScreenshot; (fs == nil || *fs) && s.actions != nil {
		// On a failed run the stage keeps pointing at what failed; the
		// final screenshot still runs for evidence.
		if firstErr == nil {
			h.update(func(r *proto.Run) { r.Stage = runStageArtifacts })
		}
		// Best-effort evidence, journaled like any screenshot action; it
		// must not mask the run's own verdict. It gets its own context so
		// a run that exhausted its budget still leaves closing evidence.
		shotCtx, cancel := context.WithTimeout(context.Background(), stepTO)
		if _, wireErr := s.dispatchAction(shotCtx, proto.ActionRequest{
			LeaseID: l.ID, Kind: "screenshot", Payload: map[string]any{"inline": false},
		}); wireErr != nil {
			s.log.Warn("run final screenshot failed", "run", h.snapshot().ID, "err", wireErr.Message)
		}
		cancel()
	}
	return firstErr
}

// runBatchStep executes a "batch" step: its payload's "actions" list runs
// through dispatchActionBatch (each entry journaled individually, exactly
// like POST /v0/actions:batch). stop_on_error defaults to true for runs.
func (s *Server) runBatchStep(ctx context.Context, leaseID string, with map[string]any) *proto.Error {
	raw, _ := with["actions"].([]any)
	if len(raw) == 0 {
		return &proto.Error{Code: proto.ErrBadRequest,
			Message: `a batch step requires with.actions (non-empty array of {kind, payload})`}
	}
	req := proto.BatchActionRequest{LeaseID: leaseID, StopOnError: true}
	if v, ok := with["stop_on_error"].(bool); ok {
		req.StopOnError = v
	}
	for i, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			return &proto.Error{Code: proto.ErrBadRequest,
				Message: fmt.Sprintf("batch actions[%d] must be an object with kind/payload", i)}
		}
		kind, _ := m["kind"].(string)
		if kind == "" {
			return &proto.Error{Code: proto.ErrBadRequest,
				Message: fmt.Sprintf("batch actions[%d]: kind is required", i)}
		}
		payload, _ := m["payload"].(map[string]any)
		req.Actions = append(req.Actions, proto.BatchAction{Kind: kind, Payload: payload})
	}
	res, wireErr := s.dispatchActionBatch(ctx, req)
	if wireErr != nil {
		return wireErr
	}
	if !res.OK {
		for i, r := range res.Results {
			if r.Error != nil {
				return prefixWireError(fmt.Sprintf("batch actions[%d]", i), r.Error)
			}
		}
		return &proto.Error{Code: proto.ErrInternal, Message: "batch failed"}
	}
	return nil
}

// runAcquire acquires the run's lease, waiting out a queue up to the
// acquire timeout. On success the granted lease is recorded on the run.
func (s *Server) runAcquire(ctx context.Context, h *runHandle, acquireTO time.Duration) (proto.Lease, *proto.Error) {
	spec := h.snapshot().Spec
	run := h.snapshot()
	purpose := "run:" + run.ID
	if spec.Name != "" {
		purpose = "run:" + spec.Name
	}
	_, runTO, _ := runTimeouts(spec)
	ttl := int(runTO.Seconds()) + runLeaseTTLMargin
	req := proto.AcquireLeaseRequest{
		Labels:  runTargetLabels(spec.Target),
		UDID:    spec.Target.UDID,
		AgentID: run.AgentID,
		Purpose: purpose,
		// clampTTL caps this at the daemon max; the release at run end
		// ends the lease long before either bound in the normal case.
		TTLSeconds: ttl,
		Reset:      spec.Target.Reset,
	}
	if s.deviceResetConflict(ctx, req) {
		return proto.Lease{}, errDeviceReset()
	}
	l, err := s.leases.Acquire(ctx, req)
	if err != nil {
		if errors.Is(err, lease.ErrNoMatch) {
			return proto.Lease{}, &proto.Error{Code: proto.ErrNoMatch, Message: noMatchMessage(req)}
		}
		return proto.Lease{}, &proto.Error{Code: proto.ErrInternal, Message: err.Error()}
	}
	// Record the lease on the run before any refusal so a rejected grant
	// still points the caller at the journal evidence.
	h.update(func(r *proto.Run) { r.LeaseID = l.ID })
	if s.refuseDeviceResetGrant(ctx, l) {
		s.syncRunOpen(l.ID)
		return proto.Lease{}, errDeviceReset()
	}
	s.syncRunOpen(l.ID)
	s.startRun(ctx, l)
	s.maybeAutoRecord(l)
	s.record(ctx, journal.Event{
		Kind: "lease", LeaseID: l.ID, AgentID: l.AgentID, Action: proto.MethodAcquireLease,
		Params: acquireParams(req),
		Status: "ok", Extra: map[string]any{"lease_state": string(l.State), "run": run.ID},
	})

	if l.State == proto.LeaseActive {
		h.update(func(r *proto.Run) { r.TargetUDID = l.TargetUDID })
		return l, nil
	}

	// Queued: wait for the grant (the lease manager promotes FIFO as
	// targets free up; the grant is journaled by the event sink).
	deadline := time.NewTimer(acquireTO)
	defer deadline.Stop()
	tick := time.NewTicker(runAcquirePoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.abandonQueuedLease(l.ID)
			return proto.Lease{}, &proto.Error{Code: proto.ErrTimeout,
				Message: "run budget exhausted while waiting for a lease"}
		case <-deadline.C:
			s.abandonQueuedLease(l.ID)
			return proto.Lease{}, &proto.Error{Code: proto.ErrTimeout,
				Message: fmt.Sprintf("no matching target became available within %s (lease was queued)", acquireTO)}
		case <-tick.C:
			cur, err := s.leases.Get(l.ID)
			if err != nil {
				return proto.Lease{}, &proto.Error{Code: proto.ErrInternal,
					Message: "queued lease disappeared: " + err.Error()}
			}
			switch cur.State {
			case proto.LeaseActive:
				if s.refuseDeviceResetGrant(ctx, cur) {
					s.syncRunOpen(cur.ID)
					return proto.Lease{}, errDeviceReset()
				}
				h.update(func(r *proto.Run) { r.TargetUDID = cur.TargetUDID })
				return cur, nil
			case proto.LeaseQueued:
				// keep waiting
			default:
				return proto.Lease{}, &proto.Error{Code: proto.ErrLeaseExpired,
					Message: fmt.Sprintf("queued lease ended (%s) before a grant", cur.State)}
			}
		}
	}
}

// errDeviceReset mirrors the acquire handler's refusal of reset-carrying
// leases that would land on a physical device (no simctl-erase equivalent).
func errDeviceReset() *proto.Error {
	return &proto.Error{Code: proto.ErrBadRequest,
		Message: "target.reset is not supported for physical devices; run without a reset"}
}

// abandonQueuedLease releases a lease the run stopped waiting on. If the
// grant raced the timeout, release still runs the lease's reset and frees
// the target.
func (s *Server) abandonQueuedLease(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.leases.Release(ctx, id); err != nil && !errors.Is(err, lease.ErrNotFound) {
		s.log.Warn("failed to release abandoned run lease", "lease", id, "err", err)
	}
	s.syncRunOpen(id)
}

// runRelease releases the run's lease (mirroring DELETE /v0/leases/{id}:
// recording drain, journal entry, pool hook, run-open reconcile).
func (s *Server) runRelease(h *runHandle, leaseID string, failed bool) {
	// A failed run keeps its failing stage through release; otherwise the
	// stage reports the release in progress (finishRun clears it on a pass).
	if !failed {
		h.update(func(r *proto.Run) { r.Stage = runStageRelease })
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	prior, priorErr := s.leases.Get(leaseID)
	// Reconcile the journal's run-open flag and attach the evidence export
	// even when the release itself fails (e.g. the lease already expired) —
	// otherwise the run folder stays exempt from GC and loses its evidence.
	defer s.syncRunOpen(leaseID)
	defer s.attachRunExport(ctx, h, leaseID)
	l, err := s.leases.Release(ctx, leaseID)
	if err != nil {
		s.log.Warn("run lease release failed", "lease", leaseID, "err", err)
		return
	}
	if priorErr == nil && prior.State == proto.LeaseActive {
		go s.stopRecordingForLease(prior, "lease_end")
	}
	if priorErr == nil && prior.State != proto.LeaseReleased && prior.State != proto.LeaseExpired {
		s.record(ctx, journal.Event{
			Kind: "lease", LeaseID: l.ID, AgentID: l.AgentID, Action: proto.MethodReleaseLease,
			Status: "ok",
		})
		if s.onLeaseEnd != nil && l.TargetUDID != "" {
			s.onLeaseEnd(l.TargetUDID)
		}
	}
}

// attachRunExport attaches the journal export to the finished run when
// requested (the default).
func (s *Server) attachRunExport(ctx context.Context, h *runHandle, leaseID string) {
	exp := h.snapshot().Spec.Artifacts.Export
	if exp != nil && !*exp {
		return
	}
	if store := s.journal.Store(); store != nil {
		if md, err := store.ExportMarkdown(ctx, leaseID); err == nil {
			h.update(func(r *proto.Run) { r.ExportMD = md })
		} else {
			s.log.Warn("run journal export failed", "run", h.snapshot().ID, "err", err)
		}
	}
}

// finishRun stamps the run's terminal state.
func (s *Server) finishRun(h *runHandle, wireErr *proto.Error) {
	now := time.Now().UTC()
	h.update(func(r *proto.Run) {
		r.FinishedAt = &now
		if wireErr != nil {
			r.State = proto.RunFailed
			r.Error = wireErr
		} else {
			r.State = proto.RunPassed
			r.Stage = ""
		}
	})
}

// recordRunEvent journals a run lifecycle entry into the run's journal.
func (s *Server) recordRunEvent(ctx context.Context, h *runHandle, l proto.Lease, action string, wireErr *proto.Error) {
	run := h.snapshot()
	ev := journal.Event{
		Kind: "run", LeaseID: l.ID, AgentID: l.AgentID, Action: action,
		Status: "ok",
		Extra:  map[string]any{"run": run.ID, "name": run.Spec.Name},
	}
	if wireErr != nil {
		ev.Status, ev.Error = "error", wireErr.Message
	}
	s.record(ctx, ev)
}

// runStageError wraps a stage failure as a wire error, classifying
// non-action errors (registry refusals, warm-pool gates, timeouts) the
// same way the individual endpoints do.
func runStageError(stage string, err error) *proto.Error {
	wireErr := actions.WireError(err)
	if wireErr.Code == proto.ErrInternal {
		if errors.Is(err, context.DeadlineExceeded) {
			wireErr.Code = proto.ErrTimeout
		} else if _, code := targetOpErr(err); code != proto.ErrInternal {
			wireErr.Code = code
		}
	}
	wireErr.Message = stage + ": " + wireErr.Message
	return wireErr
}

// runStateError classifies state-engine errors the same way
// writeStateError does for the individual endpoints.
func runStateError(stage string, err error) *proto.Error {
	code := proto.ErrInternal
	switch {
	case errors.Is(err, state.ErrTargetBusy):
		code = proto.ErrTargetBusy
	case errors.Is(err, state.ErrSnapshotNotFound):
		code = proto.ErrNotFound
	case errors.Is(err, state.ErrBadFixture), errors.Is(err, state.ErrBadReset):
		code = proto.ErrBadRequest
	case errors.Is(err, context.DeadlineExceeded):
		code = proto.ErrTimeout
	}
	return &proto.Error{Code: code, Message: stage + ": " + err.Error()}
}

// prefixWireError prefixes a wire error's message with its stage/step.
func prefixWireError(prefix string, e *proto.Error) *proto.Error {
	return &proto.Error{Code: e.Code, Message: prefix + ": " + e.Message,
		Detail: e.Detail, RetryAfterSeconds: e.RetryAfterSeconds}
}
