package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/record"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// SetRecorderManager wires the video recorder manager. When unset, the
// recording endpoints return 501 not_implemented.
func (s *Server) SetRecorderManager(m *record.Manager) {
	s.recorder = m
	// Watchdog stops (max_seconds / max_bytes / child exit) are
	// daemon-initiated: no explicit caller owns the result, so the
	// manager hands it to us for ingest.
	m.OnAutoStop(func(res record.Result) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.ingestRecording(ctx, "", res)
	})
}

// handleRecordingStart implements POST /v0/targets/{udid}/recording.
func (s *Server) handleRecordingStart(w http.ResponseWriter, r *http.Request) {
	if s.recorder == nil {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented,
			"recording is not implemented in this build")
		return
	}
	udid := r.PathValue("udid")
	var req proto.RecordingRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	l, ok := s.requireLease(w, req.LeaseID, udid)
	if !ok {
		return
	}
	info, err := s.startRecording(r.Context(), l, req.Codec, req.MaxSeconds)
	if err != nil {
		status, code := recordingErr(err)
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, proto.Recording{
		ID: info.ID, TargetUDID: info.UDID, Codec: info.Codec,
		MaxSeconds: info.MaxSeconds, StartedAt: info.StartedAt,
	})
}

// handleRecordingStop implements POST /v0/targets/{udid}/recording/stop.
func (s *Server) handleRecordingStop(w http.ResponseWriter, r *http.Request) {
	if s.recorder == nil {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented,
			"recording is not implemented in this build")
		return
	}
	udid := r.PathValue("udid")
	var req proto.RecordingStopRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	l, ok := s.requireLease(w, req.LeaseID, udid)
	if !ok {
		return
	}
	res, first, err := s.recorder.StopRun(udid, l.ID, "stopped")
	if err != nil {
		writeError(w, http.StatusConflict, proto.ErrTargetBusy, err.Error())
		return
	}
	if !first {
		// A concurrent stop (auto-stop or another caller) won the race and
		// owns the ingest; the entry will appear in the journal.
		writeError(w, http.StatusConflict, proto.ErrTargetBusy,
			"recording was already being stopped")
		return
	}
	out := s.ingestRecording(r.Context(), l.AgentID, res)
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

// recordingErr maps a recorder start failure to HTTP status + error code.
func recordingErr(err error) (int, string) {
	var nf *registry.NotFoundError
	switch {
	case errors.Is(err, record.ErrAlreadyRecording),
		errors.Is(err, record.ErrPoisoned),
		errors.Is(err, errTargetNotBooted):
		return http.StatusConflict, proto.ErrTargetBusy
	case errors.Is(err, record.ErrTooManyRecordings),
		errors.Is(err, record.ErrDiskTooLow):
		return http.StatusServiceUnavailable, proto.ErrOverloaded
	case errors.Is(err, record.ErrJournalRequired):
		return http.StatusNotImplemented, proto.ErrNotImplemented
	case errors.Is(err, record.ErrBadCodec):
		return http.StatusBadRequest, proto.ErrBadRequest
	case errors.As(err, &nf):
		return http.StatusNotFound, proto.ErrNotFound
	default:
		// Registry lookups or the manager shutting down: a daemon-side
		// fault the client may retry, not a malformed request.
		return http.StatusInternalServerError, proto.ErrInternal
	}
}

var errTargetNotBooted = errors.New("target is not booted")

// startRecording checks the target is Booted (recordVideo against a
// non-Booted sim "succeeds" with a 0-byte file) then starts the recorder
// and journals the start.
func (s *Server) startRecording(ctx context.Context, l proto.Lease, codec string, maxSeconds int) (record.Info, error) {
	t, err := s.reg.Get(ctx, l.TargetUDID)
	if err != nil {
		return record.Info{}, err
	}
	if t.State != proto.StateBooted {
		return record.Info{}, fmt.Errorf("%w (state %s)", errTargetNotBooted, t.State)
	}
	info, err := s.recorder.Start(l.TargetUDID, l.ID, codec, maxSeconds)
	if err != nil {
		return record.Info{}, err
	}
	s.record(ctx, journal.Event{
		Kind: "action", LeaseID: l.ID, AgentID: l.AgentID,
		Action: proto.MethodRecordingStart, Status: "ok",
		Params: map[string]any{"udid": l.TargetUDID, "recording_id": info.ID,
			"codec": info.Codec, "max_seconds": info.MaxSeconds},
	})
	return info, nil
}

// ingestRecording moves a finalized spool into the run's content-addressed
// artifact store and appends the segment entry. It goes straight through
// the journal store (not the HTTP artifact endpoint) so video is not
// subject to the 64 MiB HTTP upload cap and so daemon-owned auto-stops can
// land their final segment after the lease is already terminal; the
// recorder's own max-bytes cap still bounds the file. Invalid recordings
// get an error-status segment entry and the spool is deleted either way.
func (s *Server) ingestRecording(ctx context.Context, agentID string, res record.Result) proto.RecordingStopResult {
	out := proto.RecordingStopResult{
		RecordingID: res.ID, Codec: res.Codec, Bytes: res.Bytes,
		DurationS: res.Duration.Seconds(), Reason: res.Reason,
	}
	store := s.journal.Store()
	ev := journal.Event{
		Kind: "segment", LeaseID: res.RunID, AgentID: agentID,
		Action: proto.MethodRecordingStop,
		Params: map[string]any{"udid": res.UDID, "recording_id": res.ID,
			"codec": res.Codec, "duration_s": out.DurationS,
			"bytes": res.Bytes, "reason": res.Reason},
	}
	if res.Err != nil {
		ev.Status = "error"
		ev.Error = res.Err.Error()
		_ = os.Remove(res.SpoolPath)
		s.cleanSpoolDir(res.SpoolPath)
		s.record(ctx, ev)
		return out
	}
	f, err := os.Open(res.SpoolPath)
	if err == nil {
		var ref journal.ArtifactRef
		ref, err = store.PutArtifact(res.RunID, "recording-"+res.ID+".mp4", f)
		f.Close()
		if err == nil {
			ev.Artifacts = []journal.ArtifactRef{ref}
			out.Artifact = &proto.RecordingArtifact{
				Path: ref.Path, SHA256: ref.SHA256, Bytes: ref.Bytes,
			}
		}
	}
	if err != nil {
		// The spool is the only copy of a validated recording; keep it so
		// a transient store failure (full disk, ...) is not a permanent
		// loss. Its path is journaled for manual retry.
		s.log.Error("recording ingest failed; spool kept",
			"run", res.RunID, "udid", res.UDID, "spool", res.SpoolPath, "err", err)
		ev.Status = "error"
		ev.Error = "ingest failed: " + err.Error()
		ev.Params["spool_path"] = res.SpoolPath
	} else {
		ev.Status = "ok"
		_ = os.Remove(res.SpoolPath)
		s.cleanSpoolDir(res.SpoolPath)
	}
	if jref := s.journal.Record(ctx, ev); jref.Seq != 0 || jref.RunID != "" {
		out.JournalRef = &jref
	}
	out.OK = ev.Status == "ok"
	return out
}

// cleanSpoolDir removes the run's tmp dir if it is now empty.
func (s *Server) cleanSpoolDir(spoolPath string) {
	if spoolPath == "" {
		return
	}
	dir := filepath.Dir(spoolPath)
	if filepath.Base(dir) == "tmp" {
		_ = os.Remove(dir) // fails (kept) unless empty
	}
}

// stopRecordingForLease auto-stops a lease's recording when the lease
// ends or its target is about to shut down / park. Scoped to the lease's
// own run so a stale asynchronous stop never cuts short a recording the
// target's next lease started. Losing the stop race is fine — the winner
// ingests. Bounded work: SIGINT + reap timeout.
func (s *Server) stopRecordingForLease(l proto.Lease, reason string) {
	if s.recorder == nil || l.TargetUDID == "" || !s.recorder.Recording(l.TargetUDID) {
		return
	}
	res, first, err := s.recorder.StopRun(l.TargetUDID, l.ID, reason)
	if err != nil || !first {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s.ingestRecording(ctx, l.AgentID, res)
}

// drainRecording stops whatever recording is live on the target — any
// owning run — and waits out an in-flight stop before returning. Used
// right before the sim goes away (shutdown, reset): proceeding while a
// previous run's recording is still finalizing would hang its recordVideo
// child and poison the sim's recording session.
func (s *Server) drainRecording(udid, reason string) {
	if s.recorder == nil || udid == "" || !s.recorder.Recording(udid) {
		return
	}
	res, first, err := s.recorder.Stop(udid, reason)
	if err != nil || !first {
		// !first: an in-flight stop won and has now completed; it ingests.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s.ingestRecording(ctx, "", res)
}

// maybeAutoRecord starts a lease's requested auto-recording once its
// target is Booted. Boots are asynchronous on real hosts, so when the
// target is still coming up this hands off to a bounded poller instead of
// giving up. Errors are logged, never surfaced: auto-record is
// best-effort and must not fail the boot/grant that triggered it. The
// actual start runs off the caller's goroutine — Manager.Start blocks
// for the early-exit probe (~2s), which must never stall a request
// handler or a WS connection's read loop.
func (s *Server) maybeAutoRecord(l proto.Lease) {
	if s.recorder == nil || l.Record != "video" || l.State != proto.LeaseActive {
		return
	}
	if run := s.recorder.RecordingRun(l.TargetUDID); run == l.ID {
		return
	} else if run != "" {
		// The previous lease's recording is still winding down; the
		// poller retries once it has finalized.
		go s.autoRecordWhenBooted(l)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := s.startRecording(ctx, l, "", 0)
		switch {
		case err == nil, errors.Is(err, record.ErrAlreadyRecording):
		case errors.Is(err, errTargetNotBooted):
			s.autoRecordWhenBooted(l)
		default:
			s.log.Warn("auto-record start failed", "lease", l.ID,
				"target", l.TargetUDID, "err", err)
		}
	}()
}

// autoRecordBootWait bounds how long auto-record waits for an
// asynchronous boot to reach Booted before giving up.
const autoRecordBootWait = 3 * time.Minute

// autoRecordPollInterval is how often the auto-record poller re-checks
// the booting target's state (a var so tests can shrink it).
var autoRecordPollInterval = 2 * time.Second

// autoRecordWhenBooted polls the target until its asynchronous boot
// reaches Booted (and any previous lease's recording has finished
// winding down), then starts the lease's requested recording. It gives
// up when the lease is no longer active or the wait deadline passes.
func (s *Server) autoRecordWhenBooted(l proto.Lease) {
	deadline := time.After(autoRecordBootWait)
	tick := time.NewTicker(autoRecordPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			s.log.Warn("auto-record abandoned: target never became recordable",
				"lease", l.ID, "target", l.TargetUDID)
			return
		case <-tick.C:
		}
		if cur, err := s.leases.Peek(l.ID); err != nil || cur.State != proto.LeaseActive {
			return
		}
		switch run := s.recorder.RecordingRun(l.TargetUDID); run {
		case l.ID:
			return
		case "":
		default:
			continue // previous lease's recording still finalizing
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := s.startRecording(ctx, l, "", 0)
		cancel()
		switch {
		case err == nil:
			return
		case errors.Is(err, errTargetNotBooted), errors.Is(err, record.ErrAlreadyRecording):
		default:
			s.log.Warn("auto-record start failed", "lease", l.ID,
				"target", l.TargetUDID, "err", err)
			return
		}
	}
}

// StopAllRecordings stops and ingests every live recording; called on
// daemon shutdown before targets are shut down or parked (a recordVideo
// child hangs if its sim shuts down mid-recording).
func (s *Server) StopAllRecordings(reason string) {
	if s.recorder == nil {
		return
	}
	for _, res := range s.recorder.StopAll(reason) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		s.ingestRecording(ctx, "", res)
		cancel()
	}
}

// RecoverRecordings reclaims recorder orphans from a previous daemon run
// (SIGINT + validate via the manager) and ingests salvageable spools with
// reason "daemon_restart". Pre-existing recordVideo strays the daemon did
// not spawn are logged, never signaled.
func (s *Server) RecoverRecordings() {
	if s.recorder == nil {
		return
	}
	s.recorder.Recover(func(res record.Result) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.ingestRecording(ctx, "", res)
	})
	// After Recover so the previous daemon's own orphan pids (named in
	// recording.json files) are not misreported as strays.
	s.recorder.LogStrays()
}
