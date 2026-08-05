// Package record manages per-target `simctl io recordVideo` children: it
// spawns one recorder per target, spools to the run's journal directory,
// stops with SIGINT + bounded reap, and validates the finalized mp4.
//
// Measured semantics this package is built around (see docs/recording.md):
//   - SIGINT finalizes a valid mp4; SIGKILL leaves a 0-byte file AND poisons
//     the sim's host recording session ("Resource busy", POSIX 16) until the
//     sim is shut down and booted again.
//   - recordVideo against a non-Booted sim exits 0 with a 0-byte file, so
//     success must be validated (bytes > 0 and a moov box).
//   - A sim shutdown mid-recording hangs the child forever; the stop path
//     therefore reaps with a timeout, never an unbounded wait.
package record

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Defaults are deliberately conservative: the primary deployment host is a
// shared daily-driver machine where a runaway recording filling the disk is
// the worst possible outcome.
const (
	DefaultMaxSeconds    = 300
	DefaultMaxBytes      = 128 << 20 // 128 MiB
	DefaultMaxConcurrent = 2
	DefaultMinFreeDisk   = 10 << 30 // 10 GiB
	// DefaultReapTimeout bounds the SIGINT → exit wait before escalating
	// to SIGKILL (which poisons the sim's recording session).
	DefaultReapTimeout = 10 * time.Second
	// defaultStartProbe is how long Start watches a fresh child for an
	// early exit: a poisoned sim's recordVideo fails within this window
	// with "Resource busy" (exit 16).
	defaultStartProbe = 2 * time.Second
	// sizePoll is how often the max-bytes watchdog stats the spool file.
	sizePoll = 5 * time.Second
)

var (
	// ErrAlreadyRecording means the target already has a live recording
	// (one recordVideo per display at a time).
	ErrAlreadyRecording = errors.New("record: target is already recording")
	// ErrNotRecording means the target has no live recording to stop.
	ErrNotRecording = errors.New("record: target is not recording")
	// ErrTooManyRecordings means the daemon-wide concurrency cap is hit.
	ErrTooManyRecordings = errors.New("record: too many concurrent recordings")
	// ErrDiskTooLow refuses new recordings below the free-disk floor.
	ErrDiskTooLow = errors.New("record: free disk below floor")
	// ErrPoisoned means the sim's host recording session is stuck busy
	// (a recorder child was SIGKILLed); shutdown + boot clears it.
	ErrPoisoned = errors.New("record: sim recording session is poisoned; shut the sim down and boot it to recover")
	// ErrJournalRequired means the manager has no journal root to spool to.
	ErrJournalRequired = errors.New("record: recording requires the journal to be enabled")
	// ErrBadCodec rejects codecs other than hevc and h264.
	ErrBadCodec = errors.New("record: unknown codec")
)

// Config configures a Manager. Zero fields take the defaults above.
type Config struct {
	Starter Starter
	// JournalRoot is the journal directory; spools live under
	// <root>/<run>/tmp/. Required.
	JournalRoot   string
	MaxSeconds    int
	MaxBytes      int64
	MaxConcurrent int
	MinFreeDisk   int64
	// FreeDisk reports free bytes on the volume holding path;
	// defaults to a statfs-based implementation.
	FreeDisk func(path string) (int64, error)
	Logger   *slog.Logger
	Now      func() time.Time
	// ReapTimeout and StartProbe override the stop-reap and early-exit
	// probe windows (tests shrink them).
	ReapTimeout time.Duration
	StartProbe  time.Duration
	// SizePoll overrides the max-bytes watchdog poll interval.
	SizePoll time.Duration
}

// Info describes one live recording.
type Info struct {
	ID         string    `json:"recording_id"`
	UDID       string    `json:"udid"`
	RunID      string    `json:"run_id"`
	Codec      string    `json:"codec"`
	MaxSeconds int       `json:"max_seconds"`
	StartedAt  time.Time `json:"started_at"`
}

// Result is a stopped recording's outcome. When Err is non-nil the spool
// failed validation (or the child had to be killed) and SpoolPath may have
// been deleted.
type Result struct {
	Info
	SpoolPath string
	Bytes     int64
	Duration  time.Duration
	Reason    string
	Err       error
}

// recording is the manager's per-target live state.
type recording struct {
	Info
	spoolPath string
	statePath string
	proc      Proc

	// ready is closed once proc is assigned (or Start failed before a
	// child existed, in which case startErr is set). doStop waits on it
	// so a stop racing Start never sees a nil proc.
	ready     chan struct{}
	readyOnce sync.Once
	startErr  error

	stopping atomic.Bool
	stopped  chan struct{} // closed when the stop sequence completes
	result   Result
	cancelWD chan struct{} // closes the watchdog goroutine
}

// Manager owns all recorder children for one daemon.
type Manager struct {
	cfg Config
	log *slog.Logger
	now func() time.Time

	mu         sync.Mutex
	byTarget   map[string]*recording
	poisoned   map[string]bool
	closed     bool
	onAutoStop func(Result)
	// statePids are pids named by recovered recording.json state files:
	// the previous daemon's own recorders, excluded from stray logging.
	statePids map[int]bool
}

// NewManager creates a Manager. Config.Starter and Config.JournalRoot are
// required.
func NewManager(cfg Config) *Manager {
	if cfg.MaxSeconds <= 0 {
		cfg.MaxSeconds = DefaultMaxSeconds
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = DefaultMaxConcurrent
	}
	if cfg.MinFreeDisk <= 0 {
		cfg.MinFreeDisk = DefaultMinFreeDisk
	}
	if cfg.FreeDisk == nil {
		cfg.FreeDisk = freeDisk
	}
	if cfg.ReapTimeout <= 0 {
		cfg.ReapTimeout = DefaultReapTimeout
	}
	if cfg.StartProbe <= 0 {
		cfg.StartProbe = defaultStartProbe
	}
	if cfg.SizePoll <= 0 {
		cfg.SizePoll = sizePoll
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Manager{
		cfg: cfg, log: log, now: now,
		byTarget: make(map[string]*recording),
		poisoned: make(map[string]bool),
	}
}

// MaxSeconds returns the effective per-recording duration cap.
func (m *Manager) MaxSeconds() int { return m.cfg.MaxSeconds }

// MaxBytes returns the effective per-recording size cap.
func (m *Manager) MaxBytes() int64 { return m.cfg.MaxBytes }

// Recording reports whether the target has a live recording.
func (m *Manager) Recording(udid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byTarget[udid] != nil
}

// RecordingRun returns the run ID that owns the target's live recording
// ("" when the target is not recording).
func (m *Manager) RecordingRun(udid string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec := m.byTarget[udid]; rec != nil {
		return rec.RunID
	}
	return ""
}

// OnAutoStop registers a callback invoked with the Result of every stop
// the manager initiated itself (watchdog reasons "max_seconds",
// "max_bytes", "exited"). Explicit Stop/StopAll callers receive their
// results directly and the callback is not invoked for them.
func (m *Manager) OnAutoStop(fn func(Result)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onAutoStop = fn
}

// ClearPoisoned clears a target's poisoned flag. Call after the sim has
// been shut down (or shutdown + booted): that cycle is the only recovery
// for a stuck host recording session.
func (m *Manager) ClearPoisoned(udid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.poisoned, udid)
}

func newRecordingID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "rec_" + hex.EncodeToString(b)
}

// Start spawns a recorder child for udid, spooling under the run's journal
// tmp dir. codec defaults to hevc; maxSeconds is clamped to the daemon cap.
func (m *Manager) Start(udid, runID, codec string, maxSeconds int) (Info, error) {
	if m.cfg.JournalRoot == "" {
		return Info{}, ErrJournalRequired
	}
	if codec == "" {
		codec = "hevc"
	}
	if codec != "hevc" && codec != "h264" {
		return Info{}, fmt.Errorf("%w: %q (hevc or h264)", ErrBadCodec, codec)
	}
	if maxSeconds <= 0 || maxSeconds > m.cfg.MaxSeconds {
		maxSeconds = m.cfg.MaxSeconds
	}
	if free, err := m.cfg.FreeDisk(m.cfg.JournalRoot); err == nil && free < m.cfg.MinFreeDisk {
		return Info{}, fmt.Errorf("%w (%d MiB free, floor %d MiB)",
			ErrDiskTooLow, free>>20, m.cfg.MinFreeDisk>>20)
	}

	id := newRecordingID()
	spoolDir := filepath.Join(m.cfg.JournalRoot, runID, "tmp")
	spoolPath := filepath.Join(spoolDir, "recording-"+id+".mp4")
	statePath := filepath.Join(spoolDir, stateFile)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Info{}, errors.New("record: manager is shut down")
	}
	if m.byTarget[udid] != nil {
		m.mu.Unlock()
		return Info{}, ErrAlreadyRecording
	}
	if m.poisoned[udid] {
		m.mu.Unlock()
		return Info{}, ErrPoisoned
	}
	if len(m.byTarget) >= m.cfg.MaxConcurrent {
		m.mu.Unlock()
		return Info{}, fmt.Errorf("%w (cap %d)", ErrTooManyRecordings, m.cfg.MaxConcurrent)
	}
	// Placeholder reserves the target while the child spawns outside the lock.
	rec := &recording{
		Info: Info{
			ID: id, UDID: udid, RunID: runID, Codec: codec,
			MaxSeconds: maxSeconds, StartedAt: m.now(),
		},
		spoolPath: spoolPath,
		statePath: statePath,
		ready:     make(chan struct{}),
		stopped:   make(chan struct{}),
		cancelWD:  make(chan struct{}),
	}
	m.byTarget[udid] = rec
	m.mu.Unlock()

	fail := func(err error) (Info, error) {
		rec.readyOnce.Do(func() {
			rec.startErr = err
			close(rec.ready)
		})
		m.mu.Lock()
		if m.byTarget[udid] == rec {
			delete(m.byTarget, udid)
		}
		m.mu.Unlock()
		return Info{}, err
	}
	if err := os.MkdirAll(spoolDir, 0o755); err != nil {
		return fail(fmt.Errorf("record: create spool dir: %w", err))
	}
	proc, err := m.cfg.Starter.Start(udid, codec, spoolPath)
	if err != nil {
		return fail(fmt.Errorf("record: start recorder: %w", err))
	}
	rec.proc = proc
	rec.readyOnce.Do(func() { close(rec.ready) })

	// Persist the state file before the early-exit probe: a daemon that
	// dies during the probe window must still be able to reclaim this
	// child on restart (recovery discovers orphans only via state files).
	if err := writeState(rec.statePath, stateData{
		RecordingID: id, PID: proc.Pid(), UDID: udid, RunID: runID,
		Codec: codec, SpoolPath: spoolPath, StartedAt: rec.StartedAt,
	}); err != nil {
		m.log.Warn("recording state file write failed", "udid", udid, "err", err)
	}

	// Probe for an early exit: a poisoned sim's recordVideo fails fast
	// with "Resource busy" (exit 16) instead of recording.
	select {
	case <-proc.Done():
		if rec.stopping.Load() {
			// A concurrent stop (lease end, daemon shutdown) already
			// won the race; it owns finalization and the result.
			return rec.Info, nil
		}
		err := proc.Err()
		_ = os.Remove(spoolPath)
		_ = os.Remove(rec.statePath)
		if exitCode(err) == 16 {
			m.mu.Lock()
			m.poisoned[udid] = true
			m.mu.Unlock()
			return fail(ErrPoisoned)
		}
		return fail(fmt.Errorf("record: recorder exited immediately: %v", err))
	case <-time.After(m.cfg.StartProbe):
	}

	if rec.stopping.Load() {
		return rec.Info, nil
	}
	go m.watchdog(rec)
	m.log.Info("recording started", "udid", udid, "run", runID, "id", id,
		"codec", codec, "max_seconds", maxSeconds)
	return rec.Info, nil
}

// watchdog enforces the duration and size caps and observes unexpected
// child exits.
func (m *Manager) watchdog(rec *recording) {
	deadline := time.NewTimer(time.Duration(rec.MaxSeconds) * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(m.cfg.SizePoll)
	defer tick.Stop()
	for {
		select {
		case <-rec.cancelWD:
			return
		case <-rec.proc.Done():
			// The child exited on its own (crash or external signal).
			m.autoStop(rec, "exited")
			return
		case <-deadline.C:
			m.autoStop(rec, "max_seconds")
			return
		case <-tick.C:
			if fi, err := os.Stat(rec.spoolPath); err == nil && fi.Size() > m.cfg.MaxBytes {
				m.autoStop(rec, "max_bytes")
				return
			}
		}
	}
}

// Stop ends the target's recording: SIGINT, reap with a timeout, SIGKILL
// (+ poisoned flag) as a last resort, then validate the spool. first is
// true for the caller whose reason won (only that caller should ingest the
// result); a concurrent Stop blocks until the sequence completes and gets
// the same Result with first == false.
func (m *Manager) Stop(udid, reason string) (Result, bool, error) {
	return m.stopTarget(udid, "", reason)
}

// StopRun is Stop scoped to one run: it refuses (ErrNotRecording) when the
// target's live recording belongs to a different run, so a stale lease-end
// stop can never cut short the next lease's recording on the same target.
func (m *Manager) StopRun(udid, runID, reason string) (Result, bool, error) {
	return m.stopTarget(udid, runID, reason)
}

func (m *Manager) stopTarget(udid, runID, reason string) (Result, bool, error) {
	m.mu.Lock()
	rec := m.byTarget[udid]
	m.mu.Unlock()
	if rec == nil || (runID != "" && rec.RunID != runID) {
		return Result{}, false, ErrNotRecording
	}
	first := m.stop(rec, reason)
	if !first {
		<-rec.stopped
	}
	return rec.result, first, nil
}

// stop runs the stop sequence exactly once; it reports whether this caller
// won (the watchdog paths funnel through here too). Losers must wait on
// rec.stopped before reading rec.result.
func (m *Manager) stop(rec *recording, reason string) bool {
	if !rec.stopping.CompareAndSwap(false, true) {
		return false
	}
	m.doStop(rec, reason)
	return true
}

// autoStop is the watchdog's stop path: when the watchdog wins the race
// it owns the result and hands it to the OnAutoStop callback for ingest.
func (m *Manager) autoStop(rec *recording, reason string) {
	if !m.stop(rec, reason) {
		return
	}
	m.mu.Lock()
	fn := m.onAutoStop
	m.mu.Unlock()
	if fn != nil {
		fn(rec.result)
	}
}

func (m *Manager) doStop(rec *recording, reason string) {
	defer close(rec.stopped)
	close(rec.cancelWD)

	<-rec.ready
	if rec.proc == nil {
		// Start failed before a child existed; nothing to signal.
		rec.result = Result{Info: rec.Info, SpoolPath: rec.spoolPath,
			Reason: reason, Err: rec.startErr}
		return
	}

	var stopErr error
	select {
	case <-rec.proc.Done():
		// Already exited (reason "exited" or an external kill).
	default:
		// SIGINT → clean finalize ("Recording completed. Writing to
		// disk."). Finalize is asynchronous: poll for exit, bounded.
		// A failed signal is not fatal on its own: it usually means the
		// child finalized and exited just before the signal landed. If
		// it really is hung, the bounded reap below reports it.
		if err := rec.proc.Signal(syscall.SIGINT); err != nil {
			m.log.Warn("signaling recorder failed (child may have already exited)",
				"udid", rec.UDID, "err", err)
		}
		select {
		case <-rec.proc.Done():
		case <-time.After(m.cfg.ReapTimeout):
			// The child hung (e.g. the sim was shut down mid-recording
			// before the SIGINT landed). SIGKILL orphans the host-side
			// recording session: mark the target poisoned so the next
			// start returns a clean error instead of exit-16 noise.
			m.log.Warn("recorder did not exit after SIGINT; killing (sim recording session now poisoned)",
				"udid", rec.UDID, "pid", rec.proc.Pid())
			_ = rec.proc.Kill()
			<-rec.proc.Done()
			m.mu.Lock()
			m.poisoned[rec.UDID] = true
			m.mu.Unlock()
			stopErr = errors.New("record: recorder killed after reap timeout")
		}
	}

	res := Result{Info: rec.Info, SpoolPath: rec.spoolPath, Reason: reason,
		Duration: m.now().Sub(rec.StartedAt), Err: stopErr}
	if fi, err := os.Stat(rec.spoolPath); err == nil {
		res.Bytes = fi.Size()
	}
	if res.Err == nil {
		res.Err = ValidateMP4(rec.spoolPath)
	}
	_ = os.Remove(rec.statePath)

	m.mu.Lock()
	if m.byTarget[rec.UDID] == rec {
		delete(m.byTarget, rec.UDID)
	}
	m.mu.Unlock()

	rec.result = res
	m.log.Info("recording stopped", "udid", rec.UDID, "run", rec.RunID,
		"id", rec.ID, "reason", reason, "bytes", res.Bytes,
		"duration", res.Duration.Round(time.Millisecond), "err", res.Err)
}

// StopAll stops every live recording (daemon shutdown: a recording left
// running would hang against a shut-down sim and orphan its host session).
// Only results whose stop this call won are returned — a concurrent stop
// (e.g. a lease-end auto-stop) owns its result and ingests it — but every
// recording is waited on before returning.
func (m *Manager) StopAll(reason string) []Result {
	m.mu.Lock()
	m.closed = true
	recs := make([]*recording, 0, len(m.byTarget))
	for _, r := range m.byTarget {
		recs = append(recs, r)
	}
	m.mu.Unlock()
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []Result
	)
	for _, r := range recs {
		wg.Add(1)
		go func(r *recording) {
			defer wg.Done()
			first := m.stop(r, reason)
			<-r.stopped
			if !first {
				return
			}
			mu.Lock()
			results = append(results, r.result)
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	return results
}

// exitCode extracts the process exit code from a Wait error (-1 if unknown).
func exitCode(err error) int {
	type coder interface{ ExitCode() int }
	var c coder
	if errors.As(err, &c) {
		return c.ExitCode()
	}
	return -1
}
