package actions

import (
	"bytes"
	"context"
	"os/exec"
)

// Runner executes external commands. Abstracted so unit tests can run
// without macOS, AXe, or simctl.
type Runner interface {
	// Run executes name with args and returns stdout and stderr. A non-nil
	// error is returned for start failures and non-zero exits.
	Run(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
}

// InputRunner is the optional stdin-feeding extension of Runner, needed by
// commands that consume standard input (e.g. `simctl pbcopy`).
type InputRunner interface {
	// RunInput executes name with args, writing stdin to the process's
	// standard input, and returns stdout and stderr.
	RunInput(ctx context.Context, stdin []byte, name string, args ...string) (stdout, stderr []byte, err error)
}

// ExecRunner runs commands via os/exec.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	return run(ctx, nil, name, args...)
}

func (ExecRunner) RunInput(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	return run(ctx, stdin, name, args...)
}

func run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}
