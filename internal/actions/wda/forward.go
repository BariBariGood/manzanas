package wda

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ForwardLauncher supervises a usbmux port forward (host localPort ->
// device remotePort) as a long-lived host-side child, the same way
// XCTestRunLauncher supervises the runner: a dead forward is
// indistinguishable from a dead runner from the client's side, so it gets
// the same launch/probe/relaunch treatment. It prefers `iproxy`
// (libimobiledevice) and falls back to `pymobiledevice3 usbmux forward`.
type ForwardLauncher struct {
	UDID   string
	Local  int
	Remote int
	// lookPath and start are overridable for tests.
	lookPath func(name string) (string, error)
	start    func(name string, args ...string) (stop func(), err error)

	mu   sync.Mutex
	stop func()
}

// ParseForward parses a "<local>:<remote>" port pair (e.g. "8100:8100").
func ParseForward(udid, spec string) (*ForwardLauncher, error) {
	l, r, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf("invalid forward spec %q for %s (want <local>:<remote>, e.g. 8100:8100)", spec, udid)
	}
	local, err := strconv.Atoi(strings.TrimSpace(l))
	if err != nil || local < 1 || local > 65535 {
		return nil, fmt.Errorf("invalid local port %q in forward spec %q", l, spec)
	}
	remote, err := strconv.Atoi(strings.TrimSpace(r))
	if err != nil || remote < 1 || remote > 65535 {
		return nil, fmt.Errorf("invalid remote port %q in forward spec %q", r, spec)
	}
	return NewForwardLauncher(udid, local, remote), nil
}

// NewForwardLauncher builds a supervised usbmux forward.
func NewForwardLauncher(udid string, local, remote int) *ForwardLauncher {
	return &ForwardLauncher{
		UDID: udid, Local: local, Remote: remote,
		lookPath: exec.LookPath,
		start:    startCommand,
	}
}

// command picks the forwarder binary available on this host.
func (f *ForwardLauncher) command() (string, []string, error) {
	if p, err := f.lookPath("iproxy"); err == nil {
		return p, []string{strconv.Itoa(f.Local), strconv.Itoa(f.Remote), "-u", f.UDID}, nil
	}
	if p, err := f.lookPath("pymobiledevice3"); err == nil {
		return p, []string{"usbmux", "forward", strconv.Itoa(f.Local), strconv.Itoa(f.Remote), "--serial", f.UDID}, nil
	}
	return "", nil, fmt.Errorf("no usbmux forwarder found for %s: install iproxy (libimobiledevice) or pymobiledevice3", f.UDID)
}

// Launch implements Launcher: any previous forward child is killed first
// so a wedged one can't hold the local port.
func (f *ForwardLauncher) Launch(ctx context.Context) error {
	f.Stop()
	name, args, err := f.command()
	if err != nil {
		return err
	}
	stop, err := f.start(name, args...)
	if err != nil {
		return fmt.Errorf("start forward %d:%d for %s: %w", f.Local, f.Remote, f.UDID, err)
	}
	f.mu.Lock()
	f.stop = stop
	f.mu.Unlock()
	return nil
}

// Stop implements Launcher: kills the running forward child, if any.
func (f *ForwardLauncher) Stop() {
	f.mu.Lock()
	stop := f.stop
	f.stop = nil
	f.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (f *ForwardLauncher) String() string {
	return fmt.Sprintf("forward:%d:%d", f.Local, f.Remote)
}

// Healthy reports whether something is listening on the local port. It is
// the forward supervisor's health probe: the forwarder listens locally
// whether or not the device is attached, so a failing dial means the
// child itself died — WDA-level health belongs to the runner supervisor.
func (f *ForwardLauncher) Healthy(ctx context.Context) bool {
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(f.Local)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
