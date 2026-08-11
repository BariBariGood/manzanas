// Package server binds the registry and lease manager to the v0 wire
// protocol over HTTP REST and WebSocket.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions"
	"github.com/BariBariGood/manzanas/internal/buildinfo"
	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/record"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/internal/state"
	"github.com/BariBariGood/manzanas/internal/stream"
	"github.com/BariBariGood/manzanas/proto"
)

// resetTimeout bounds a post-lease auto-reset (shutdown + erase or
// data-dir swap of a multi-GB device directory).
const resetTimeout = 10 * time.Minute

// redactLeaseID maps a lease ID to a short one-way digest for the
// shutdown ledger and its sinks (logs, journal, client-visible detail):
// lease IDs are v0 capability tokens, so no part of the raw ID may leak,
// but the holder of an ID can compute the same digest to correlate a
// shutdown record with their lease. Mirrors the broker's redaction.
func redactLeaseID(id string) string {
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return "sha:" + hex.EncodeToString(sum[:4])
}

// Server serves the manzanasd v0 protocol.
type Server struct {
	reg      registry.Registry
	leases   *lease.Manager
	state    state.Engine // nil when the host has no simctl (endpoints 501)
	images   state.Images // nil when the host has no simctl (endpoints 501)
	streamer *stream.Manager
	log      *slog.Logger
	events   *eventHub
	actions  actions.Backend   // nil until an actions backend is wired
	journal  *journal.Recorder // nil (no-op) until the journal is wired
	recorder *record.Manager   // nil until video recording is wired (endpoints 501)
	// devicesEnabled marks that the registry may contain physical
	// device targets (--devices); guards that only exist to protect
	// phones stay out of simulator-only deployments.
	devicesEnabled bool
	// parked reports whether a target is SIGSTOPped in the warm pool;
	// nil means no pool. Streams refuse parked targets (frame capture
	// against a frozen tree hangs) — acquiring a lease thaws them.
	// Target listings mark parked targets Warm for schedulers.
	parked func(udid string) bool
	// poolStatus snapshots the warm pool for /v0/status; nil = no pool.
	poolStatus PoolStatusFunc
	// onLeaseEnd fires when a lease is explicitly released (expiry goes
	// through the lease manager's event sink instead); nil = no pool.
	// The pool uses it to reclaim daemon-booted non-pool sims.
	onLeaseEnd func(udid string)
	// advice is the latest scheduler advisory accepted via
	// POST /v0/pool/advise; informational only (see handleAdvisePool).
	adviceMu sync.Mutex
	advice   *proto.PoolAdviceState
	// dashReadonly disables the dashboard's mutating control endpoints
	// (/v0/dash/...); see SetDashReadonly.
	dashReadonly bool
	// authToken, when non-empty, gates every endpoint except GET
	// /v0/healthz and static assets behind a shared bearer token; see
	// SetAuthToken.
	authToken string

	// bootWaitPoll overrides the boot-wait retry interval (tests);
	// 0 means defaultBootWaitPoll.
	bootWaitPoll time.Duration
	// bootWaitSlots caps concurrent ?wait=true boot waits (maxBootWaiters).
	bootWaitSlots chan struct{}
	// bootGates is a cheap boot-gate pre-check (no full target listing)
	// the boot-wait retry loop probes before each full boot attempt so
	// waiting stays cheap on an overloaded host; nil = no pre-check.
	bootGates func(udid string) error
	// waitMu guards waitByLease, the set of lease IDs with a boot wait
	// in flight: one concurrent wait per lease, so a single holder can't
	// pin every waiter slot with repeated ?wait=true requests.
	waitMu      sync.Mutex
	waitByLease map[string]bool

	// downMu guards lastShutdown, the per-target record of the most
	// recent daemon-driven shutdown (who and why). Action errors against
	// a not-booted leased target surface it (see targetDownDetail).
	downMu       sync.Mutex
	lastShutdown map[string]shutdownNote

	// openMu serializes run open/close transitions so the journal's
	// GC-exempt flag always converges to the lease's current state (see
	// syncRunOpen).
	openMu sync.Mutex

	// runs tracks the one-call run primitive's resources (POST /v0/runs).
	runs *runStore
}

// New creates a Server. The lease manager may be nil at construction time
// (so its GrantFunc can be wired to EventSink) and set later via SetLeases.
func New(reg registry.Registry, leases *lease.Manager, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{reg: reg, leases: leases, log: log, events: newEventHub(),
		lastShutdown:  make(map[string]shutdownNote),
		bootWaitSlots: make(chan struct{}, maxBootWaiters),
		waitByLease:   make(map[string]bool),
		runs:          newRunStore()}
}

// shutdownNote records one daemon-driven target shutdown for later
// surfacing (actor is who decided, e.g. "janitor", "watchdog", "agent
// <id>"; reason is why).
type shutdownNote struct {
	Actor  string
	Reason string
	At     time.Time
}

// NoteShutdown records that the daemon (or one of its subsystems) shut a
// target down, so a later target_not_booted error can tell the leaseholder
// who did it and why. When the target is held by an active lease the
// shutdown is also journaled into that lease's run.
func (s *Server) NoteShutdown(udid, actor, reason string) {
	if udid == "" {
		return
	}
	now := time.Now().UTC()
	s.downMu.Lock()
	s.lastShutdown[udid] = shutdownNote{Actor: actor, Reason: reason, At: now}
	s.downMu.Unlock()
	s.log.Info("target shutdown noted", "udid", udid, "actor", actor, "reason", reason)
	if s.leases == nil {
		return
	}
	if l, ok := s.leases.Active(udid); ok {
		s.record(context.Background(), journal.Event{
			Kind: "target", LeaseID: l.ID, AgentID: l.AgentID,
			Action: "target.shutdown", Status: "ok",
			Extra: map[string]any{"udid": udid, "actor": actor, "reason": reason},
		})
	}
}

// clearShutdownNote drops a target's shutdown ledger entry (called on a
// successful boot: the shutdown it recorded has been undone, so a later
// not-booted error must not report it as the cause).
func (s *Server) clearShutdownNote(udid string) {
	s.downMu.Lock()
	delete(s.lastShutdown, udid)
	s.downMu.Unlock()
}

// NoteBoot invalidates a target's shutdown ledger entry: the daemon has
// observed the target reach Booted, so the recorded shutdown was undone.
// The warm pool wires it as its boot reporter — pool boots (rePark after
// a post-lease reset, Recycle rebuilds, BootAsync) never pass through the
// server's own boot handler.
func (s *Server) NoteBoot(udid string) { s.clearShutdownNote(udid) }

// targetDownDetail explains why a target is not booted, from the daemon's
// shutdown ledger. Always returns a non-empty, actionable sentence.
func (s *Server) targetDownDetail(udid string) string {
	s.downMu.Lock()
	n, ok := s.lastShutdown[udid]
	s.downMu.Unlock()
	if !ok {
		return "the daemon has no record of shutting this target down; it was shut down externally (simctl, a crash, or before the daemon started)"
	}
	return "target was shut down by " + n.Actor + " at " + n.At.Format(time.RFC3339) + ": " + n.Reason
}

// SetLeases wires the lease manager; must be called before Handler serves
// traffic if leases was nil in New.
func (s *Server) SetLeases(m *lease.Manager) { s.leases = m }

// SetParkedCheck wires the warm pool's parked query; may stay nil when
// there is no pool.
func (s *Server) SetParkedCheck(fn func(udid string) bool) { s.parked = fn }

// SetBootGates wires a cheap boot-gate pre-check (e.g. the warm pool's
// cached-count gate) that the ?wait=true retry loop probes before each
// full boot attempt, so a waiting client doesn't force a full simctl
// listing per poll on an already-overloaded host.
func (s *Server) SetBootGates(fn func(udid string) error) { s.bootGates = fn }

// SetLeaseEndHook wires a callback fired after an explicit lease release
// (expired leases reach the pool through the lease manager's event sink);
// may stay nil when there is no pool.
func (s *Server) SetLeaseEndHook(fn func(udid string)) { s.onLeaseEnd = fn }

// SetState wires the state engine; may stay nil, in which case the state
// endpoints return not_implemented.
func (s *Server) SetState(e state.Engine) { s.state = e }

// SetImages wires the golden-image store; may stay nil, in which case the
// image endpoints return not_implemented.
func (s *Server) SetImages(st state.Images) { s.images = st }

// SetDevicesEnabled marks that physical device targets may exist in the
// registry (--devices), enabling the device-protection guards.
func (s *Server) SetDevicesEnabled(on bool) { s.devicesEnabled = on }

// ResetSink returns a lease.ResetFunc that runs each ended lease's
// requested auto-reset through the state engine. Pass it to
// lease.Manager.SetResetFunc when a state engine is wired. A returned
// error makes the manager quarantine the target instead of handing it out.
func (s *Server) ResetSink() func(proto.Lease) error {
	return func(l proto.Lease) error {
		// A reset shuts the sim down (erase / snapshot swap); a recordVideo
		// child hangs if its sim goes away mid-recording, so stop the
		// lease's recording — and drain any other run's still-finalizing
		// one — first. This runs on the reset goroutine — lease release
		// itself never blocks on mp4 finalize.
		s.stopRecordingForLease(l, "lease_end")
		s.drainRecording(l.TargetUDID, "target_shutdown")
		if s.state == nil {
			return nil
		}
		// Bound the reset so a hung simctl becomes a logged failure plus
		// quarantine instead of holding the target forever.
		ctx, cancel := context.WithTimeout(context.Background(), resetTimeout)
		defer cancel()
		// Acquire refuses reset specs on device leases, but a label set
		// can match a device without naming it; never run a simctl reset
		// against a physical device. Only when devices are enabled: a
		// confirmed device skips the reset (journaled); an indeterminate
		// lookup — which could be masking a device behind an unhealthy
		// sub-registry — quarantines the target rather than erasing a
		// maybe-phone or handing the next holder a dirty simulator. On
		// simulator-only daemons no lookup happens at all.
		if s.devicesEnabled {
			t, gerr := s.reg.Get(ctx, l.TargetUDID)
			if gerr == nil && t.Kind == proto.TargetDevice {
				s.log.Warn("skipping post-lease reset: target is a physical device",
					"lease", l.ID, "target", l.TargetUDID, "reset", l.Reset)
				s.recordState(ctx, l.ID, "state.reset", map[string]any{
					"udid": l.TargetUDID, "reset": l.Reset, "skipped": "physical device",
				}, nil)
				return nil
			}
			if gerr != nil {
				s.log.Error("post-lease reset: target kind indeterminate; quarantining",
					"lease", l.ID, "target", l.TargetUDID, "reset", l.Reset, "err", gerr)
				s.recordState(ctx, l.ID, "state.reset", map[string]any{
					"udid": l.TargetUDID, "reset": l.Reset,
				}, gerr)
				return gerr
			}
		}
		// The reset shuts the sim down; ledger it so a holder poking a
		// mid-reset (or freshly reset) target gets an actionable detail.
		s.NoteShutdown(l.TargetUDID, "daemon", "auto-reset ("+l.Reset+") for lease "+redactLeaseID(l.ID))
		err := s.state.Reset(ctx, l.TargetUDID, l.Reset)
		params := map[string]any{"udid": l.TargetUDID, "reset": l.Reset}
		defer func() { s.recordState(ctx, l.ID, "state.reset", params, err) }()
		if errors.Is(err, state.ErrSnapshotNotFound) {
			// The snapshot never existed or was deleted mid-lease. Degrade
			// to an erase: the next holder still gets a clean target and a
			// typo can't park the target in quarantine.
			s.log.Warn("lease auto-reset snapshot missing; degrading to erase",
				"lease", l.ID, "target", l.TargetUDID, "reset", l.Reset, "err", err)
			// Record what actually ran, not just what was requested.
			params["degraded_to"] = state.ResetErase
			params["degraded_reason"] = err.Error()
			err = s.state.Reset(ctx, l.TargetUDID, state.ResetErase)
		}
		if err != nil {
			s.log.Error("lease auto-reset failed; target quarantined",
				"lease", l.ID, "target", l.TargetUDID, "reset", l.Reset, "err", err)
			return err
		}
		// A reset cycles the sim through a shutdown, which clears a
		// wedged host recording session.
		if s.recorder != nil {
			s.recorder.ClearPoisoned(l.TargetUDID)
		}
		return nil
	}
}

// SetStreamer wires the media streamer. If never called, stream routes
// return 501 not_implemented (capability probing per PROTOCOL.md).
func (s *Server) SetStreamer(m *stream.Manager) { s.streamer = m }

// SetActions wires the actions backend. When unset, /v0/actions returns
// not_implemented.
func (s *Server) SetActions(b actions.Backend) { s.actions = b }

// SetJournal wires the run journal; every protocol action on a lease is
// then recorded to it. A nil recorder disables journaling.
func (s *Server) SetJournal(rec *journal.Recorder) { s.journal = rec }

// record journals one protocol action; best-effort, never fails the op.
func (s *Server) record(ctx context.Context, ev journal.Event) {
	s.journal.Record(ctx, ev)
}

// syncRunOpen reconciles a run's GC-exempt flag with the lease's current
// state: open while the lease is active, closed otherwise. Lease events
// are emitted outside the lease manager's lock, so acquire/grant/release/
// expiry notifications can arrive out of order; every one of them calls
// this after its state change commits, and openMu serializes the
// read-state-then-set-flag step, so the last transition always leaves the
// flag matching the lease's true state (never permanently open for a
// terminal lease, never closed for a live one). Peek (not Get) is used
// because Get can synchronously emit lease events, which would re-enter
// this function on the same goroutine and self-deadlock on openMu.
func (s *Server) syncRunOpen(leaseID string) {
	store := s.journal.Store()
	if store == nil || leaseID == "" {
		return
	}
	s.openMu.Lock()
	defer s.openMu.Unlock()
	if l, err := s.leases.Peek(leaseID); err == nil && l.State == proto.LeaseActive {
		store.MarkOpen(leaseID)
		return
	}
	store.MarkClosed(leaseID)
}

// startRun captures determinism metadata when a lease becomes active.
func (s *Server) startRun(ctx context.Context, l proto.Lease) {
	if s.journal.Store() == nil || l.State != proto.LeaseActive {
		return
	}
	var target *proto.Target
	if t, err := s.reg.Get(ctx, l.TargetUDID); err == nil {
		target = &t
	}
	s.journal.StartRun(l, target, proto.Version)
}

// EventSink returns a lease.GrantFunc that fans lease events out to
// connected WS clients. Pass it to lease.New.
func (s *Server) EventSink() func(proto.Lease) {
	return func(l proto.Lease) {
		event := proto.EventLeaseExpired
		switch {
		case l.State == proto.LeaseActive && l.GraceUntil != nil:
			// Still active, past nominal expiry: a one-shot warning that
			// the renewal grace window is open.
			event = proto.EventLeaseExpiring
		case l.State == proto.LeaseActive:
			event = proto.EventLeaseGranted
		}
		// The lease entry is appended synchronously (a local file append)
		// so it always precedes entries for actions taken in reaction to
		// this event. Only startRun is deferred: it hits the registry,
		// which may shell out (simctl), and this callback runs on the
		// lease manager's event path (including its expiry loop), which
		// must never block on the registry.
		if store := s.journal.Store(); store != nil {
			// Journal under a protocol-method-style action name (the
			// payload's `action` key documents method names, not WS event
			// names); the raw event name rides along for stream consumers.
			action := "leases.grant"
			switch event {
			case proto.EventLeaseExpired:
				action = "leases.expire"
			case proto.EventLeaseExpiring:
				action = "leases.expiring"
			}
			// The run is open (GC-exempt) for exactly the lease's lifetime:
			// reconciled at grant (here and in the acquire handlers) and at
			// lease end.
			if event == proto.EventLeaseGranted {
				s.syncRunOpen(l.ID)
			} else {
				defer s.syncRunOpen(l.ID)
			}
			s.record(context.Background(), journal.Event{
				Kind: "lease", LeaseID: l.ID, AgentID: l.AgentID,
				Action: action, Status: "ok",
				Extra: map[string]any{"event": event},
			})
			if event == proto.EventLeaseGranted {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					s.startRun(ctx, l)
					s.maybeAutoRecord(l)
				}()
			}
		}
		if event == proto.EventLeaseExpired {
			// The lease's recording must not outlive the lease: stop it
			// (SIGINT + bounded reap) and ingest off the expiry loop's
			// goroutine — never block the lease manager on mp4 finalize.
			go s.stopRecordingForLease(l, "lease_end")
		}
		s.events.broadcast(event, l)
	}
}

// Handler returns the HTTP handler for all v0 routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/healthz", s.handleHealthz)
	// Read-only fleet dashboard (embedded static assets). /dash redirects
	// to /dash/ so relative asset URLs resolve.
	dash := dashHandler()
	mux.Handle("GET /dash/", dash)
	mux.Handle("GET /dash", http.RedirectHandler("/dash/", http.StatusMovedPermanently))
	// Dashboard control endpoints: leaseless target lifecycle ops plus
	// release-by-target and stop-recording-by-target (lease IDs are
	// capability tokens the dashboard never holds). All mutations are
	// refused with 403 read_only under --dash-readonly.
	mux.HandleFunc("GET /v0/dash/config", s.handleDashConfig)
	mux.HandleFunc("POST /v0/dash/targets/{udid}/boot", s.handleDashBoot)
	mux.HandleFunc("POST /v0/dash/targets/{udid}/shutdown", s.handleDashShutdown)
	mux.HandleFunc("POST /v0/dash/targets/{udid}/release", s.handleDashReleaseLease)
	mux.HandleFunc("POST /v0/dash/targets/{udid}/recording/stop", s.handleDashRecordingStop)
	mux.HandleFunc("GET /v0/status", s.handleStatus)
	mux.HandleFunc("POST /v0/pool/advise", s.handleAdvisePool)
	mux.HandleFunc("GET /v0/targets", s.handleListTargets)
	mux.HandleFunc("POST /v0/targets/{udid}/boot", s.handleBoot)
	mux.HandleFunc("POST /v0/targets/{udid}/shutdown", s.handleShutdown)
	mux.HandleFunc("POST /v0/targets/{udid}/clear-quarantine", s.handleClearQuarantine)
	mux.HandleFunc("POST /v0/targets/{udid}/recording", s.handleRecordingStart)
	mux.HandleFunc("POST /v0/targets/{udid}/recording/stop", s.handleRecordingStop)
	mux.HandleFunc("POST /v0/leases", s.handleAcquireLease)
	mux.HandleFunc("GET /v0/leases", s.handleListLeases)
	mux.HandleFunc("GET /v0/leases/{id}", s.handleGetLease)
	mux.HandleFunc("POST /v0/leases/{id}/renew", s.handleRenewLease)
	mux.HandleFunc("DELETE /v0/leases/{id}", s.handleReleaseLease)
	mux.HandleFunc("GET /v0/ws", s.handleWS)
	mux.HandleFunc("GET /v0/journal", s.handleJournalList)
	mux.HandleFunc("GET /v0/journal/{run}", s.handleJournalGet)
	mux.HandleFunc("GET /v0/journal/{run}/export.md", s.handleJournalExport)
	mux.HandleFunc("GET /v0/journal/{run}/artifacts/{path...}", s.handleJournalArtifactGet)
	mux.HandleFunc("POST /v0/journal/{run}/artifacts", s.handleJournalArtifactPut)
	// Catch-all so unknown /v0/journal/... paths still answer with the
	// documented JSON error shape (more specific patterns win).
	mux.HandleFunc("/v0/journal/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "unknown journal route")
	})
	if s.state != nil {
		mux.HandleFunc("POST /v0/state/snapshots", s.handleStateSnapshot)
		mux.HandleFunc("GET /v0/state/snapshots", s.handleStateSnapshotsList)
		mux.HandleFunc("DELETE /v0/state/snapshots/{id}", s.handleStateSnapshotsDelete)
		mux.HandleFunc("POST /v0/state/restore", s.handleStateRestore)
		mux.HandleFunc("POST /v0/state/erase", s.handleStateErase)
		mux.HandleFunc("POST /v0/state/fixtures", s.handleStateFixture)
	}
	// Catch-all so unknown /v0/state/... paths still answer with the
	// documented JSON error shape (more specific patterns win).
	mux.HandleFunc("/v0/state/", s.notImplemented("state"))
	if s.images != nil {
		mux.HandleFunc("POST /v0/images/build", s.handleImagesBuild)
		mux.HandleFunc("GET /v0/images", s.handleImagesList)
		mux.HandleFunc("POST /v0/images/{id}/stamp", s.handleImagesStamp)
		mux.HandleFunc("DELETE /v0/images/{id}", s.handleImagesDelete)
	} else {
		mux.HandleFunc("GET /v0/images", s.notImplemented("images"))
	}
	mux.HandleFunc("/v0/images/", s.notImplemented("images"))
	if s.streamer != nil {
		mux.HandleFunc("POST /v0/streams", s.corsStreams(s.handleOpenStream))
		mux.HandleFunc("DELETE /v0/streams/{id}", s.handleCloseStream)
		mux.HandleFunc("GET /v0/streams/{id}/mjpeg", s.handleStreamMJPEG)
		mux.HandleFunc("GET /v0/streams/{id}/ws", s.handleStreamWS)
		mux.HandleFunc("GET /view/{udid}", s.handleViewPage)
	} else {
		mux.HandleFunc("POST /v0/streams", s.corsStreams(s.notImplemented("streams")))
	}
	mux.HandleFunc("OPTIONS /v0/streams", s.handleStreamsPreflight)
	mux.HandleFunc("POST /v0/actions", s.handleAction)
	mux.HandleFunc("POST /v0/actions:batch", s.handleActionBatch)
	mux.HandleFunc("POST /v0/runs", s.handleRunCreate)
	mux.HandleFunc("GET /v0/runs", s.handleRunList)
	mux.HandleFunc("GET /v0/runs/{id}", s.handleRunGet)
	return s.authMiddleware(mux)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": proto.Version, "build": buildinfo.Version,
	})
}

func (s *Server) notImplemented(slice string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented,
			slice+" is not implemented in this build")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, proto.Error{Code: code, Message: msg})
}

// writeWireError writes a full proto.Error (detail, retry hint included),
// mirroring RetryAfterSeconds in the standard Retry-After header.
func writeWireError(w http.ResponseWriter, status int, e *proto.Error) {
	if e.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfterSeconds))
	}
	writeJSON(w, status, e)
}

// maxJSONBodyBytes bounds a JSON request body (1 MiB); journal entries
// derived from request payloads stay well under the reader's line limit.
const maxJSONBodyBytes = 1 << 20

// decodeJSON decodes the request body into v; an empty body leaves v at its
// zero value (all request fields have documented defaults).
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(v)
	if err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
