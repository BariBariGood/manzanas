package wda

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
)

// XCTestRunLauncher runs WebDriverAgent through
// `xcodebuild test-without-building -xctestrun <path> -destination id=<udid>`,
// the long-running host-side session that installs and drives the runner.
// The .xctestrun comes from a one-time `xcodebuild build-for-testing` on
// this host (which is also where signing happens).
type XCTestRunLauncher struct {
	UDID string
	Path string
	// start spawns a long-running command, returning a kill func;
	// overridable for tests.
	start func(name string, args ...string) (stop func(), err error)

	mu   sync.Mutex
	stop func()
}

// NewXCTestRunLauncher builds a launcher for an .xctestrun bundle path.
func NewXCTestRunLauncher(udid, path string) *XCTestRunLauncher {
	return &XCTestRunLauncher{UDID: udid, Path: path, start: startCommand}
}

func startCommand(name string, args ...string) (func(), error) {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Reap the child when it exits (or is killed) so it never zombies.
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	return func() {
		_ = cmd.Process.Kill()
		<-done
	}, nil
}

// Launch implements Launcher: any previous xcodebuild session is killed
// first so a wedged one can't hold the device.
func (l *XCTestRunLauncher) Launch(ctx context.Context) error {
	l.Stop()
	stop, err := l.start("xcodebuild", "test-without-building",
		"-xctestrun", l.Path, "-destination", "id="+l.UDID)
	if err != nil {
		return fmt.Errorf("xcodebuild test-without-building %s for %s: %w", l.Path, l.UDID, err)
	}
	l.mu.Lock()
	l.stop = stop
	l.mu.Unlock()
	return nil
}

// Stop implements Launcher: kills the running xcodebuild session, if any.
func (l *XCTestRunLauncher) Stop() {
	l.mu.Lock()
	stop := l.stop
	l.stop = nil
	l.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (l *XCTestRunLauncher) String() string {
	return "xctestrun:" + l.Path
}
