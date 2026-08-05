package record

import (
	"os"
	"os/exec"
	"syscall"
)

// Proc is one running recorder child process.
type Proc interface {
	Pid() int
	// Signal delivers sig to the child (SIGINT triggers a clean finalize).
	Signal(sig os.Signal) error
	// Kill force-terminates the child (SIGKILL). Killing a recordVideo
	// child poisons the simulator's host recording session until the sim
	// is shut down and booted again.
	Kill() error
	// Done is closed once the child has exited; the error (if any) is the
	// exit error, readable via Err after Done is closed.
	Done() <-chan struct{}
	// Err returns the exit error once Done is closed (nil on exit 0).
	Err() error
}

// Starter spawns one recorder child writing to outPath. Abstracted so unit
// tests run without macOS or simctl.
type Starter interface {
	Start(udid, codec, outPath string) (Proc, error)
}

// execProc wraps an exec.Cmd as a Proc.
type execProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func newExecProc(cmd *exec.Cmd) (*execProc, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

func (p *execProc) Pid() int                   { return p.cmd.Process.Pid }
func (p *execProc) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }
func (p *execProc) Kill() error                { return p.cmd.Process.Kill() }
func (p *execProc) Done() <-chan struct{}      { return p.done }
func (p *execProc) Err() error                 { return p.err }

// SimctlStarter runs `xcrun simctl io <udid> recordVideo` (macOS hosts).
type SimctlStarter struct{}

func (SimctlStarter) Start(udid, codec, outPath string) (Proc, error) {
	args := []string{"simctl", "io", udid, "recordVideo", "--codec=" + codec, "--force", outPath}
	cmd := exec.Command("xcrun", args...)
	// Own process group so signals target exactly this child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return newExecProc(cmd)
}

// mockMarker tags mock recorder children so orphan recovery can identify
// them by cmdline, mirroring the "recordVideo" + UDID match on macOS.
const mockMarker = "manzanasd-mock-recordVideo"

// MockStarter spawns a shell child that idles until SIGINT/SIGTERM, then
// writes a minimal valid mp4 (ftyp + moov boxes) to outPath and exits 0 —
// enough for `--mock` daemons and Linux restart-recovery testing.
type MockStarter struct{}

func (MockStarter) Start(udid, codec, outPath string) (Proc, error) {
	// The UDID rides along as a positional argument (never interpolated
	// into the script) so it appears on the cmdline for orphan matching
	// without being a shell-injection sink.
	script := `finish(){ printf '\0\0\0\020ftypisom\0\0\0\0\0\0\0\010moov' > "$1"; exit 0; }; : ` +
		mockMarker + ` "$1"; trap 'finish "$0"' INT TERM; while :; do sleep 1; done`
	cmd := exec.Command("/bin/sh", "-c", script, outPath, udid)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return newExecProc(cmd)
}
