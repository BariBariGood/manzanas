package warm

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

const (
	// DefaultLockDir is where the fleet lock protocol keeps its
	// per-resource lock files (one `sim-<udid>` file per simulator).
	DefaultLockDir = "/tmp/manzanas_locks"
	// lockStaleAge mirrors the fleet lock protocol: a lock untouched for
	// this long is stale and its resource may be taken over.
	lockStaleAge = 2 * time.Hour
	// lockReapGrace is how long an unmanaged sim must stay observed with
	// a stale (or missing) lock across consecutive sweeps before it is
	// shut down: an agent that just created/claimed the sim but hasn't
	// written its lock yet must not be raced.
	lockReapGrace = 10 * time.Minute
)

// lockState classifies a sim's fleet lock file.
type lockState int

const (
	lockFresh lockState = iota
	lockStale
	lockMissing
	lockMalformed
)

func (s lockState) String() string {
	switch s {
	case lockFresh:
		return "fresh"
	case lockStale:
		return "stale"
	case lockMissing:
		return "missing"
	default:
		return "malformed"
	}
}

// simLockState inspects `<dir>/sim-<udid>`. The lock protocol writes one
// line, `<session-id> <ISO timestamp> <task>`; freshness uses the newer
// of the recorded timestamp and the file's mtime (claim/refresh update
// both, and a bare touch still counts as activity). Anything unreadable
// or unparsable is malformed — callers fail closed and treat it as
// fresh.
func simLockState(dir, udid string, now time.Time) lockState {
	path := filepath.Join(dir, "sim-"+udid)
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return lockMissing
	}
	if err != nil {
		return lockMalformed
	}
	f, err := os.Open(path)
	if err != nil {
		return lockMalformed
	}
	// A protocol lock is one short line; bound the read so a bogus huge
	// file can't balloon memory.
	data := make([]byte, 4096)
	n, err := io.ReadFull(f, data)
	f.Close()
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return lockMalformed
	}
	data = data[:n]
	line, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return lockMalformed
	}
	ts, err := time.Parse(time.RFC3339, fields[1])
	if err != nil {
		return lockMalformed
	}
	newest := ts
	if m := fi.ModTime(); m.After(newest) {
		newest = m
	}
	if now.Sub(newest) < lockStaleAge {
		return lockFresh
	}
	return lockStale
}

// reapStaleLocked sweeps the unmanaged Booted sims and shuts down (never
// deletes) those whose fleet lock has been stale or missing for a full
// grace period. A fresh lock always protects the sim; a malformed lock
// fails closed (treated as fresh).
func (p *Pool) reapStaleLocked(ctx context.Context, unmanaged []proto.Target, now time.Time) {
	// A missing/unreadable lock dir means the lock protocol's signal is
	// unavailable for every sim at once (mistyped --lock-dir, dir never
	// created): fail closed and skip the whole pass rather than treating
	// each lock as missing and mass-shutting sims down.
	if !p.lockDirUsable() {
		p.resetReapMarks()
		return
	}
	current := make(map[string]bool, len(unmanaged))
	for _, t := range unmanaged {
		// A sim configured for the warm pool but not yet adopted (startup
		// provisioning is asynchronous and can outlast the grace) is the
		// daemon's own, not an abandoned agent sim.
		if p.isConfiguredPool(t.UDID) {
			continue
		}
		current[t.UDID] = true
		st := simLockState(p.cfg.LockDir, t.UDID, now)
		switch st {
		case lockFresh:
			p.clearStaleSince(t.UDID)
			p.logLockDecision(t, st, "fresh lock; not touched", false)
		case lockMalformed:
			p.clearStaleSince(t.UDID)
			p.logLockDecision(t, st, "malformed lock; failing closed (treated as fresh)", true)
		case lockStale, lockMissing:
			// A sim in active use must not accumulate grace: clear the
			// mark so a full fresh grace runs after the activity ends
			// (mirrors clearIdleSince for daemon-booted sims).
			if p.isLeased(t.UDID) || p.isBusy(t.UDID) || p.isAwake(t.UDID) ||
				p.isStreaming(t.UDID) || p.isRecording(t.UDID) {
				p.clearStaleSince(t.UDID)
				p.logLockDecision(t, st, "in active use; grace reset", false)
				continue
			}
			since := p.staleSinceOrMark(t.UDID, now)
			if now.Sub(since) < lockReapGrace {
				p.logLockDecision(t, st, "reap grace pending", false)
				continue
			}
			octx, cancel := context.WithTimeout(ctx, janitorOpTimeout)
			reaped := p.reapUnmanaged(octx, t.UDID)
			cancel()
			if reaped {
				p.clearStaleSince(t.UDID)
				p.mu.Lock()
				p.reapedN++
				delete(p.lastLockDecision, t.UDID)
				p.mu.Unlock()
				p.log.Info("janitor: shut down unmanaged sim with stale/missing fleet lock",
					"udid", t.UDID, "name", t.Name, "lock", st.String())
			}
		}
	}
	// Drop marks for sims that left the unmanaged-Booted set (shut down,
	// deleted, leased, or adopted): a later reappearance starts a fresh
	// grace with fresh logging.
	p.mu.Lock()
	for u := range p.staleSince {
		if !current[u] {
			delete(p.staleSince, u)
		}
	}
	for u := range p.lastLockDecision {
		if !current[u] {
			delete(p.lastLockDecision, u)
		}
	}
	p.mu.Unlock()
}

// reapUnmanaged shuts an unmanaged Booted sim down if it is still safe
// to: not pooled, not daemon-booted (those belong to reclaimIdle), not
// leased/busy/awake/streaming/recording, and — re-checked at the last
// moment — still without a fresh lock. Reports whether the sim was shut
// down.
func (p *Pool) reapUnmanaged(ctx context.Context, udid string) bool {
	if p.isConfiguredPool(udid) || p.IsMember(udid) || p.IsDaemonBooted(udid) || p.isLeased(udid) ||
		p.isBusy(udid) || p.isAwake(udid) || p.isStreaming(udid) || p.isRecording(udid) {
		return false
	}
	// Re-check the lock right before acting: an agent claiming the sim
	// between the sweep's observation and this shutdown must win, and a
	// lock dir that vanished mid-sweep must abort the reap.
	if !p.lockDirUsable() {
		return false
	}
	if st := simLockState(p.cfg.LockDir, udid, time.Now()); st == lockFresh || st == lockMalformed {
		return false
	}
	st, err := p.reg.Health(ctx, udid)
	var nf *registry.NotFoundError
	if errors.As(err, &nf) {
		return false
	}
	if err != nil || st != proto.StateBooted {
		return false
	}
	release, takeover, ok := p.reserveTarget(udid)
	if !ok {
		return false // leased since the checks above
	}
	defer func() { release(!takeover) }()
	if p.isStreaming(udid) || p.isRecording(udid) || p.isBusy(udid) {
		return false
	}
	if err := p.SafeShutdown(ctx, udid); err != nil {
		p.log.Warn("janitor: stale-lock reap shutdown failed", "udid", udid, "err", err)
		return false
	}
	p.reportShutdown(udid, "janitor", "unmanaged sim reaped: fleet lock stale or missing")
	return true
}

// resetReapMarks drops all grace marks and decision memory when a sweep
// is aborted (lock dir outage, registry listing failure): a mark that
// survives an outage could reap a sim recreated under the same UDID on
// the first sweep instead of after a fresh grace.
func (p *Pool) resetReapMarks() {
	p.mu.Lock()
	clear(p.staleSince)
	clear(p.lastLockDecision)
	p.mu.Unlock()
}

// lockDirUsable reports whether the configured lock dir exists and is a
// directory, warning once per outage when it is not.
func (p *Pool) lockDirUsable() bool {
	fi, err := os.Stat(p.cfg.LockDir)
	usable := err == nil && fi.IsDir()
	p.mu.Lock()
	warned := p.lockDirWarned
	p.lockDirWarned = !usable
	p.mu.Unlock()
	if !usable && !warned {
		p.log.Warn("janitor: lock dir missing or not a directory; skipping stale-lock reaping",
			"lock_dir", p.cfg.LockDir, "err", err)
	}
	return usable
}

// staleSinceOrMark returns when the sim's lock was first observed
// stale/missing, marking it now on the first observation.
func (p *Pool) staleSinceOrMark(udid string, now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.staleSince[udid]; ok {
		return t
	}
	p.staleSince[udid] = now
	return now
}

func (p *Pool) clearStaleSince(udid string) {
	p.mu.Lock()
	delete(p.staleSince, udid)
	p.mu.Unlock()
}

// logLockDecision logs one reap decision per state change (a stuck sim
// must not spam every sweep). warn selects the log level.
func (p *Pool) logLockDecision(t proto.Target, st lockState, msg string, warn bool) {
	key := st.String() + ":" + msg
	p.mu.Lock()
	same := p.lastLockDecision[t.UDID] == key
	p.lastLockDecision[t.UDID] = key
	p.mu.Unlock()
	if same {
		return
	}
	if warn {
		p.log.Warn("janitor: unmanaged sim lock decision: "+msg,
			"udid", t.UDID, "name", t.Name, "lock", st.String())
		return
	}
	p.log.Info("janitor: unmanaged sim lock decision: "+msg,
		"udid", t.UDID, "name", t.Name, "lock", st.String())
}
