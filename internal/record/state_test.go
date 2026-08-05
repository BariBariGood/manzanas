package record

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func writeStateFor(t *testing.T, root, runID string, st stateData) string {
	t.Helper()
	dir := filepath.Join(root, runID, "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if st.SpoolPath == "" {
		st.SpoolPath = filepath.Join(dir, "recording-"+st.RecordingID+".mp4")
	}
	if err := writeState(filepath.Join(dir, stateFile), st); err != nil {
		t.Fatal(err)
	}
	return st.SpoolPath
}

func TestRecoverDeadPidValidSpool(t *testing.T) {
	m, _, root := testManager(t, nil)
	spool := writeStateFor(t, root, "run-1", stateData{
		RecordingID: "rec_dead", PID: 1, UDID: "SIM-1", RunID: "run-1",
		Codec: "hevc", StartedAt: time.Now().Add(-time.Minute),
	})
	if err := os.WriteFile(spool, validMP4, 0o644); err != nil {
		t.Fatal(err)
	}
	// pid 1's cmdline is not a recorder, so recovery must not signal it —
	// only salvage the spool.
	var got []Result
	m.Recover(func(r Result) { got = append(got, r) })
	if len(got) != 1 {
		t.Fatalf("ingested %d results, want 1", len(got))
	}
	r := got[0]
	if r.Reason != "daemon_restart" || r.UDID != "SIM-1" || r.Bytes != int64(len(validMP4)) {
		t.Fatalf("result = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(root, "run-1", "tmp", stateFile)); !os.IsNotExist(err) {
		t.Fatal("state file not removed")
	}
}

func TestRecoverInvalidSpoolDeleted(t *testing.T) {
	m, _, root := testManager(t, nil)
	spool := writeStateFor(t, root, "run-1", stateData{
		RecordingID: "rec_bad", PID: 1, UDID: "SIM-1", RunID: "run-1",
	})
	if err := os.WriteFile(spool, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m.Recover(func(r Result) { t.Fatalf("invalid spool ingested: %+v", r) })
	if _, err := os.Stat(spool); !os.IsNotExist(err) {
		t.Fatal("invalid spool not deleted")
	}
}

func TestRecoverSignalsLiveOrphan(t *testing.T) {
	m, _, root := testManager(t, nil)
	dir := filepath.Join(root, "run-1", "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(dir, "recording-rec_live.mp4")
	// A real orphan: the MockStarter child writes a valid mp4 on SIGINT.
	proc, err := MockStarter{}.Start("SIM-1", "hevc", spool)
	if err != nil {
		t.Fatal(err)
	}
	// Give the shell child time to install its SIGINT trap.
	time.Sleep(300 * time.Millisecond)
	writeStateFor(t, root, "run-1", stateData{
		RecordingID: "rec_live", PID: proc.Pid(), UDID: "SIM-1", RunID: "run-1",
		Codec: "hevc", SpoolPath: spool, StartedAt: time.Now(),
	})
	saveReap := m.cfg.ReapTimeout
	m.cfg.ReapTimeout = 5 * time.Second // the sh child sleeps up to 1s
	defer func() { m.cfg.ReapTimeout = saveReap }()

	var got []Result
	m.Recover(func(r Result) { got = append(got, r) })
	if len(got) != 1 {
		t.Fatalf("ingested %d results, want 1", len(got))
	}
	if got[0].Reason != "daemon_restart" || got[0].Bytes == 0 {
		t.Fatalf("result = %+v", got[0])
	}
	select {
	case <-proc.Done():
	case <-time.After(time.Second):
		t.Fatal("orphan child still running")
	}
}

func TestRecoverPidReuseNotSignaled(t *testing.T) {
	// A live process whose cmdline is NOT a recorder: recovery must not
	// signal it and must still salvage/delete the spool.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	m, _, root := testManager(t, nil)
	spool := writeStateFor(t, root, "run-1", stateData{
		RecordingID: "rec_reuse", PID: cmd.Process.Pid, UDID: "SIM-1", RunID: "run-1",
	})
	if err := os.WriteFile(spool, validMP4, 0o644); err != nil {
		t.Fatal(err)
	}
	var got []Result
	m.Recover(func(r Result) { got = append(got, r) })
	if len(got) != 1 {
		t.Fatalf("ingested %d results, want 1", len(got))
	}
	// The stranger process must still be alive (never signaled).
	if cmdlineOS(cmd.Process.Pid) == "" {
		t.Fatal("stranger process was killed during recovery")
	}
}
