package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// stateFile is the per-run recorder state file (<run>/tmp/recording.json):
// enough to recover an orphaned child after a daemon restart.
const stateFile = "recording.json"

type stateData struct {
	RecordingID string    `json:"recording_id"`
	PID         int       `json:"pid"`
	UDID        string    `json:"udid"`
	RunID       string    `json:"run_id"`
	Codec       string    `json:"codec"`
	SpoolPath   string    `json:"spool_path"`
	StartedAt   time.Time `json:"started_at"`
}

func writeState(path string, d stateData) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// Cmdline returns the command line of a live process ("" when the pid is
// gone). Overridable for tests.
var Cmdline = cmdlineOS

// Recover scans the journal root for recording.json state files left by a
// previous daemon and reclaims each orphan:
//   - pid alive AND its cmdline matches a recorder we spawned (recordVideo /
//     the mock marker, plus the state file's UDID): SIGINT it, reap bounded,
//     validate, and hand the Result to ingest (reason "daemon_restart").
//   - pid gone or cmdline mismatch (pid reuse): never signal — a stranger's
//     process must not be killed; salvage the spool if it parses, else
//     delete it.
//
// ingest is only called for spools that validate; invalid spools are
// deleted. State files are removed either way.
func (m *Manager) Recover(ingest func(Result)) {
	if m.cfg.JournalRoot == "" {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(m.cfg.JournalRoot, "*", "tmp", stateFile))
	for _, statePath := range matches {
		raw, err := os.ReadFile(statePath)
		if err != nil {
			continue
		}
		var st stateData
		if err := json.Unmarshal(raw, &st); err != nil || st.SpoolPath == "" ||
			!spoolUnderRoot(m.cfg.JournalRoot, st.SpoolPath) ||
			st.RunID != filepath.Base(filepath.Dir(filepath.Dir(statePath))) {
			// A spool path outside the journal root — or a run_id that
			// does not name the state file's own run directory — is never
			// something we wrote: refuse to read or delete it.
			_ = os.Remove(statePath)
			continue
		}
		m.mu.Lock()
		if m.statePids == nil {
			m.statePids = make(map[int]bool)
		}
		m.statePids[st.PID] = true
		m.mu.Unlock()
		m.recoverOne(statePath, st, ingest)
	}
}

// spoolUnderRoot reports whether path resolves to somewhere inside root.
func spoolUnderRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (m *Manager) recoverOne(statePath string, st stateData, ingest func(Result)) {
	log := m.log.With("udid", st.UDID, "run", st.RunID, "pid", st.PID)
	cmdline := Cmdline(st.PID)
	// A recorder we spawned always carries the spool path on its cmdline
	// (simctl arg / mock argv), so require it alongside the UDID before
	// treating the pid as ours.
	ours := cmdline != "" && strings.Contains(cmdline, st.UDID) &&
		strings.Contains(cmdline, st.SpoolPath) &&
		(strings.Contains(cmdline, "recordVideo") || strings.Contains(cmdline, mockMarker))
	switch {
	case ours:
		log.Info("recovering orphaned recorder child (SIGINT)")
		_ = syscall.Kill(st.PID, syscall.SIGINT)
		if !waitPidGone(st.PID, m.cfg.ReapTimeout) {
			// Never SIGKILL here: it would poison the sim's recording
			// session, and unlike our own children we cannot be sure a
			// hung pid is safe to kill. Leave it and the spool alone.
			log.Warn("orphaned recorder did not exit after SIGINT; leaving it running")
			return
		}
	case cmdline != "":
		// Pid is alive but is not our recorder (pid reuse): log only.
		log.Warn("recording.json pid is now a different process; not signaling", "cmdline", cmdline)
	}

	res := Result{
		Info: Info{ID: st.RecordingID, UDID: st.UDID, RunID: st.RunID,
			Codec: st.Codec, StartedAt: st.StartedAt},
		SpoolPath: st.SpoolPath,
		Reason:    "daemon_restart",
	}
	if fi, err := os.Stat(st.SpoolPath); err == nil {
		res.Bytes = fi.Size()
		// Wall clock minus StartedAt would include the whole daemon
		// downtime; the spool's mtime (last write) bounds the actual
		// recorded length far better. Clamp to the duration cap.
		if d := fi.ModTime().Sub(st.StartedAt); d > 0 {
			if max := time.Duration(m.cfg.MaxSeconds) * time.Second; d > max {
				d = max
			}
			res.Duration = d
		}
	}
	res.Err = ValidateMP4(st.SpoolPath)
	_ = os.Remove(statePath)
	if res.Err != nil {
		log.Warn("orphaned spool failed validation; deleting", "err", res.Err)
		_ = os.Remove(st.SpoolPath)
		return
	}
	log.Info("orphaned recording salvaged", "bytes", res.Bytes)
	ingest(res)
}

// waitPidGone polls until the pid no longer exists (true) or the timeout
// elapses (false).
func waitPidGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) != nil
}

// LogStrays logs recordVideo processes that predate the daemon and have no
// state file. They may belong to another agent on a shared host, so they
// are never signaled — surfaced for a human to triage. Call after
// Recover: pids named by a recording.json are the daemon's own orphans,
// not strays, and are skipped here.
func (m *Manager) LogStrays() {
	m.mu.Lock()
	known := m.statePids
	m.mu.Unlock()
	for _, pid := range findRecordVideoPids() {
		if known[pid] {
			continue
		}
		m.log.Warn("stray recordVideo process (not started by this daemon); leaving it alone", "pid", pid)
	}
}
