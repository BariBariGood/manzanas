package eval

import (
	"context"
	"fmt"
)

// stepExecutor runs one scenario step op. Each op lives in its own op_*.go
// file; the ops registry below wires them together.
type stepExecutor interface {
	// Execute runs the step. detail is a short human-readable note for the
	// report (may be empty).
	Execute(ctx context.Context, rc *runContext, st *Step) (detail string, err error)
}

// runContext is the per-run state shared between step executors.
type runContext struct {
	client    *Client
	scenario  string            // scenario name (namespaces artifact filenames)
	lease     string            // active lease ID
	udid      string            // leased target UDID
	run       int               // 1-based run number
	saved     map[string]string // tree hashes saved via tree_hash.save_as
	artDir    string            // directory for artifacts (screenshots); "" = don't save
	profile   TimingProfile     // host-class timing profile (zero = identity)
	artifacts []string          // artifact paths recorded this run
	snapshots []string          // snapshot IDs created this run (deleted at teardown)
}

// ops is the registry of step executors keyed by Step.Op.
var ops = map[string]stepExecutor{
	"boot":     bootOp{},
	"shutdown": shutdownOp{},
	"action":   actionOp{},
	"fixture":  fixtureOp{},
	"snapshot": snapshotOp{},
	"restore":  restoreOp{},
	"erase":    eraseOp{},
	"wait":     waitOp{},
	"assert":   assertOp{},
}

func executorFor(op string) (stepExecutor, error) {
	ex, ok := ops[op]
	if !ok {
		return nil, fmt.Errorf("unknown op %q", op)
	}
	return ex, nil
}
