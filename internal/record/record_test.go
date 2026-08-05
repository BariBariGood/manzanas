package record

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// validMP4 is a minimal container the validator accepts: a 16-byte ftyp
// box followed by an 8-byte moov box.
var validMP4 = []byte("\x00\x00\x00\x10ftypisom\x00\x00\x00\x00\x00\x00\x00\x08moov")

// fakeProc is a scriptable recorder child.
type fakeProc struct {
	pid       int
	spool     string
	onSIGINT  func(p *fakeProc) // default: write validMP4 and exit
	ignoreInt bool              // simulate a hung child (sim shut down)

	mu     sync.Mutex
	done   chan struct{}
	exited bool
	err    error
}

func newFakeProc(pid int, spool string) *fakeProc {
	return &fakeProc{pid: pid, spool: spool, done: make(chan struct{})}
}

func (p *fakeProc) exit(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return
	}
	p.exited = true
	p.err = err
	close(p.done)
}

func (p *fakeProc) Pid() int { return p.pid }
func (p *fakeProc) Signal(sig os.Signal) error {
	if sig != syscall.SIGINT || p.ignoreInt {
		return nil
	}
	if p.onSIGINT != nil {
		p.onSIGINT(p)
		return nil
	}
	_ = os.WriteFile(p.spool, validMP4, 0o644)
	p.exit(nil)
	return nil
}
func (p *fakeProc) Kill() error {
	// SIGKILL leaves whatever bytes were spooled (usually zero).
	p.exit(errors.New("killed"))
	return nil
}
func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) Err() error            { return p.err }

// fakeStarter records starts and hands out fakeProcs.
type fakeStarter struct {
	mu      sync.Mutex
	nextPid int
	started []*fakeProc
	// prepare tweaks each proc before it is returned.
	prepare func(p *fakeProc)
	// startErr fails the spawn itself.
	startErr error
}

func (s *fakeStarter) Start(udid, codec, outPath string) (Proc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startErr != nil {
		return nil, s.startErr
	}
	s.nextPid++
	p := newFakeProc(s.nextPid, outPath)
	if s.prepare != nil {
		s.prepare(p)
	}
	s.started = append(s.started, p)
	return p, nil
}

func testManager(t *testing.T, mutate func(*Config)) (*Manager, *fakeStarter, string) {
	t.Helper()
	root := t.TempDir()
	st := &fakeStarter{}
	cfg := Config{
		Starter:     st,
		JournalRoot: root,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		StartProbe:  10 * time.Millisecond,
		ReapTimeout: 200 * time.Millisecond,
		SizePoll:    20 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewManager(cfg), st, root
}

func TestStartStopHappyPath(t *testing.T) {
	m, st, root := testManager(t, nil)
	info, err := m.Start("SIM-1", "run-1", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if info.Codec != "hevc" || info.MaxSeconds != DefaultMaxSeconds {
		t.Fatalf("info = %+v", info)
	}
	if !m.Recording("SIM-1") {
		t.Fatal("Recording(SIM-1) = false")
	}
	statePath := filepath.Join(root, "run-1", "tmp", stateFile)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file: %v", err)
	}

	res, first, err := m.Stop("SIM-1", "stopped")
	if err != nil || !first {
		t.Fatalf("Stop: %v first=%v", err, first)
	}
	if res.Err != nil {
		t.Fatalf("result err: %v", res.Err)
	}
	if res.Reason != "stopped" || res.Bytes != int64(len(validMP4)) {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatal("state file not removed")
	}
	if m.Recording("SIM-1") {
		t.Fatal("still recording after stop")
	}
	_ = st
}

func TestStopNotRecording(t *testing.T) {
	m, _, _ := testManager(t, nil)
	if _, _, err := m.Stop("SIM-1", "stopped"); !errors.Is(err, ErrNotRecording) {
		t.Fatalf("err = %v", err)
	}
}

func TestStopRunScopedToOwner(t *testing.T) {
	m, _, _ := testManager(t, nil)
	if _, err := m.Start("SIM-1", "run-1", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.StopRun("SIM-1", "run-other", "lease_end"); !errors.Is(err, ErrNotRecording) {
		t.Fatalf("foreign-run stop err = %v", err)
	}
	if !m.Recording("SIM-1") {
		t.Fatal("foreign-run stop terminated the recording")
	}
	if got := m.RecordingRun("SIM-1"); got != "run-1" {
		t.Fatalf("RecordingRun = %q", got)
	}
	if _, first, err := m.StopRun("SIM-1", "run-1", "stopped"); err != nil || !first {
		t.Fatalf("owner stop: %v first=%v", err, first)
	}
}

func TestOneRecordingPerTarget(t *testing.T) {
	m, _, _ := testManager(t, nil)
	if _, err := m.Start("SIM-1", "run-1", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start("SIM-1", "run-1", "", 0); !errors.Is(err, ErrAlreadyRecording) {
		t.Fatalf("err = %v", err)
	}
}

func TestConcurrencyCap(t *testing.T) {
	m, _, _ := testManager(t, func(c *Config) { c.MaxConcurrent = 1 })
	if _, err := m.Start("SIM-1", "run-1", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start("SIM-2", "run-2", "", 0); !errors.Is(err, ErrTooManyRecordings) {
		t.Fatalf("err = %v", err)
	}
}

func TestDiskFloor(t *testing.T) {
	m, _, _ := testManager(t, func(c *Config) {
		c.FreeDisk = func(string) (int64, error) { return 1 << 30, nil }
	})
	if _, err := m.Start("SIM-1", "run-1", "", 0); !errors.Is(err, ErrDiskTooLow) {
		t.Fatalf("err = %v", err)
	}
}

func TestBadCodec(t *testing.T) {
	m, _, _ := testManager(t, nil)
	if _, err := m.Start("SIM-1", "run-1", "vp9", 0); err == nil {
		t.Fatal("want codec error")
	}
}

func TestZeroByteSpoolFailsValidation(t *testing.T) {
	m, _, _ := testManager(t, func(c *Config) {
		// Child "finalizes" a 0-byte file (recordVideo against a
		// Shutdown sim exits 0 with an empty file).
		c.Starter = &fakeStarter{prepare: func(p *fakeProc) {
			p.onSIGINT = func(p *fakeProc) {
				_ = os.WriteFile(p.spool, nil, 0o644)
				p.exit(nil)
			}
		}}
	})
	if _, err := m.Start("SIM-1", "run-1", "", 0); err != nil {
		t.Fatal(err)
	}
	res, _, err := m.Stop("SIM-1", "stopped")
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(res.Err, ErrInvalidRecording) {
		t.Fatalf("result err = %v, want ErrInvalidRecording", res.Err)
	}
}

func TestHungChildIsKilledAndPoisons(t *testing.T) {
	m, _, _ := testManager(t, func(c *Config) {
		c.Starter = &fakeStarter{prepare: func(p *fakeProc) { p.ignoreInt = true }}
	})
	if _, err := m.Start("SIM-1", "run-1", "", 0); err != nil {
		t.Fatal(err)
	}
	res, _, err := m.Stop("SIM-1", "stopped")
	if err != nil {
		t.Fatal(err)
	}
	if res.Err == nil {
		t.Fatal("want kill error in result")
	}
	// The target is now poisoned until shutdown+boot.
	if _, err := m.Start("SIM-1", "run-1", "", 0); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("err = %v, want ErrPoisoned", err)
	}
	m.ClearPoisoned("SIM-1")
	if _, err := m.Start("SIM-1", "run-1", "", 0); err != nil {
		t.Fatalf("after ClearPoisoned: %v", err)
	}
}

func TestEarlyExit16MarksPoisoned(t *testing.T) {
	m, _, _ := testManager(t, func(c *Config) {
		c.Starter = &fakeStarter{prepare: func(p *fakeProc) {
			p.exit(exit16Err{})
		}}
	})
	if _, err := m.Start("SIM-1", "run-1", "", 0); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("err = %v, want ErrPoisoned", err)
	}
	// The flag persists for the next start too.
	if _, err := m.Start("SIM-1", "run-1", "", 0); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("second start err = %v, want ErrPoisoned", err)
	}
}

type exit16Err struct{}

func (exit16Err) Error() string { return "exit status 16" }
func (exit16Err) ExitCode() int { return 16 }

func TestConcurrentStopSharesResult(t *testing.T) {
	m, _, _ := testManager(t, nil)
	if _, err := m.Start("SIM-1", "run-1", "", 0); err != nil {
		t.Fatal(err)
	}
	type out struct {
		res   Result
		first bool
		err   error
	}
	results := make(chan out, 2)
	for i := 0; i < 2; i++ {
		go func() {
			res, first, err := m.Stop("SIM-1", "race")
			results <- out{res, first, err}
		}()
	}
	firsts := 0
	for i := 0; i < 2; i++ {
		o := <-results
		if o.err != nil {
			// The loser may arrive after the stop completed and the
			// target was deregistered; that reads as not-recording.
			if errors.Is(o.err, ErrNotRecording) {
				continue
			}
			t.Fatalf("stop: %v", o.err)
		}
		if o.first {
			firsts++
		}
		if o.res.Reason != "race" {
			t.Fatalf("reason = %q", o.res.Reason)
		}
	}
	if firsts != 1 {
		t.Fatalf("firsts = %d, want exactly 1", firsts)
	}
}

func TestMaxSecondsWatchdog(t *testing.T) {
	m, st, _ := testManager(t, nil)
	if _, err := m.Start("SIM-1", "run-1", "", 1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return !m.Recording("SIM-1") })
	st.mu.Lock()
	p := st.started[0]
	st.mu.Unlock()
	select {
	case <-p.Done():
	default:
		t.Fatal("child not stopped by watchdog")
	}
}

func TestWatchdogStopInvokesOnAutoStop(t *testing.T) {
	m, _, _ := testManager(t, nil)
	got := make(chan Result, 1)
	m.OnAutoStop(func(r Result) { got <- r })
	if _, err := m.Start("SIM-1", "run-1", "", 1); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if r.Reason != "max_seconds" {
			t.Fatalf("reason = %q", r.Reason)
		}
		if r.Err != nil {
			t.Fatalf("result err: %v", r.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnAutoStop not invoked")
	}
}

func TestStopDuringSlowStartDoesNotPanic(t *testing.T) {
	release := make(chan struct{})
	m, _, _ := testManager(t, func(c *Config) {
		c.Starter = starterFunc(func(udid, codec, outPath string) (Proc, error) {
			<-release
			p := newFakeProc(1, outPath)
			return p, nil
		})
	})
	errc := make(chan error, 1)
	go func() {
		_, err := m.Start("SIM-1", "run-1", "", 0)
		errc <- err
	}()
	waitFor(t, 5*time.Second, func() bool { return m.Recording("SIM-1") })
	stopc := make(chan error, 1)
	go func() {
		_, _, err := m.Stop("SIM-1", "lease_end")
		stopc <- err
	}()
	time.Sleep(50 * time.Millisecond) // let Stop block on readiness
	close(release)
	if err := <-errc; err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := <-stopc; err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// starterFunc adapts a function to the Starter interface.
type starterFunc func(udid, codec, outPath string) (Proc, error)

func (f starterFunc) Start(udid, codec, outPath string) (Proc, error) {
	return f(udid, codec, outPath)
}

func TestMaxBytesWatchdog(t *testing.T) {
	m, _, _ := testManager(t, func(c *Config) { c.MaxBytes = 10 })
	if _, err := m.Start("SIM-1", "run-1", "", 0); err != nil {
		t.Fatal(err)
	}
	// The fake child only writes on SIGINT; grow the spool by hand so the
	// size watchdog trips.
	m.mu.Lock()
	rec := m.byTarget["SIM-1"]
	m.mu.Unlock()
	if err := os.WriteFile(rec.spoolPath, make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return !m.Recording("SIM-1") })
}

func TestStopAll(t *testing.T) {
	m, _, _ := testManager(t, nil)
	for i := 1; i <= 2; i++ {
		if _, err := m.Start(fmt.Sprintf("SIM-%d", i), fmt.Sprintf("run-%d", i), "", 0); err != nil {
			t.Fatal(err)
		}
	}
	results := m.StopAll("daemon_shutdown")
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for _, r := range results {
		if r.Reason != "daemon_shutdown" || r.Err != nil {
			t.Fatalf("result = %+v", r)
		}
	}
	// New starts are refused after StopAll.
	if _, err := m.Start("SIM-3", "run-3", "", 0); err == nil {
		t.Fatal("start after StopAll should fail")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
