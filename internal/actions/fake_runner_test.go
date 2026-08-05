package actions

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
)

// fakeRunner records invocations and replays canned responses keyed by the
// first argument (the AXe/simctl subcommand), so tests run without macOS.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string

	// stdout maps subcommand -> stdout to return.
	stdout map[string]string
	// errs maps subcommand -> error to return (with stderr text).
	errs map[string]string
	// failFirst maps subcommand -> remaining number of failures before
	// the call succeeds, used to exercise retry paths.
	failFirst map[string]int
	// writeFile maps subcommand -> file content written to the path that
	// follows the "--output" flag (or the last argument), emulating tools
	// that produce artifacts.
	writeFile map[string]string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		stdout:    map[string]string{},
		errs:      map[string]string{},
		failFirst: map[string]int{},
		writeFile: map[string]string{},
	}
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))

	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "simctl" && len(args) > 1 {
		sub = args[1]
	}
	if n := f.failFirst[sub]; n > 0 {
		f.failFirst[sub] = n - 1
		return nil, []byte("No translation object returned for AXTraits"), fmt.Errorf("exit status 1")
	}
	if msg, ok := f.errs[sub]; ok {
		return nil, []byte(msg), fmt.Errorf("exit status 1")
	}
	if content, ok := f.writeFile[sub]; ok {
		if err := os.WriteFile(outputPath(args), []byte(content), 0o600); err != nil {
			return nil, nil, err
		}
	}
	return []byte(f.stdout[sub]), nil, nil
}

// RunInput implements InputRunner: stdin is recorded as a trailing
// "<stdin:...>" argv entry so tests can assert on it.
func (f *fakeRunner) RunInput(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	return f.Run(ctx, name, append(args, "<stdin:"+string(stdin)+">")...)
}

// outputPath finds the artifact path in an argv: the value after
// "--output", else the last argument that looks like a path.
func outputPath(args []string) string {
	for i, a := range args {
		if a == "--output" && i+1 < len(args) {
			return args[i+1]
		}
	}
	for i := len(args) - 1; i >= 0; i-- {
		if strings.HasPrefix(args[i], "/") {
			return args[i]
		}
	}
	return ""
}

// argvs returns the recorded calls as space-joined strings.
func (f *fakeRunner) argvs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

// testBackend builds an AXeBackend over the fake runner with AXe present.
func testBackend(f *fakeRunner) *AXeBackend {
	return NewAXe(WithRunner(f), WithAXePath("/fake/axe"))
}
