package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/BariBariGood/manzanas/internal/runspec"
	"github.com/BariBariGood/manzanas/proto"
)

// cmdRun implements `manzanas run spec.yaml`: the one-call run primitive
// (POST /v0/runs). The daemon executes the whole loop — lease, boot,
// fixtures, install, launch, steps, artifacts, release — from the
// declarative spec; evidence lands in the run journal (run ID = lease ID).
//
//	manzanas run spec.yaml                 # sync: waits for the verdict
//	manzanas run spec.yaml --async         # returns the run ID immediately
//	manzanas run --status RUN_ID           # poll an async run
//	manzanas run --ls                      # list retained runs
func cmdRun(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("run")
	agentID := fs.String("agent", "", "agent ID (default $USER)")
	async := fs.Bool("async", false, "return immediately; poll with --status")
	status := fs.String("status", "", "fetch a run by ID instead of starting one")
	ls := fs.Bool("ls", false, "list retained runs")
	exportOut := fs.String("o", "", "write the journal export markdown to FILE (- for stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	// Allow flags after the spec path too (`run spec.yaml -o out.md`).
	if len(rest) > 0 {
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		if fs.NArg() > 0 {
			return fmt.Errorf("run: unexpected argument %q (one spec path per run)", fs.Arg(0))
		}
	}
	if (*ls || *status != "") && len(rest) > 0 {
		return fmt.Errorf("run: --ls/--status cannot be combined with a spec path")
	}
	if *ls {
		runs, err := app.client.ListRuns(ctx)
		if err != nil {
			return err
		}
		return app.emit(map[string]any{"runs": runs}, func(w io.Writer) {
			for _, r := range runs {
				fmt.Fprintf(w, "%s  %-7s  %s  steps=%d  %s\n", r.ID, r.State,
					r.CreatedAt.Format(time.RFC3339), len(r.Steps), r.Spec.Name)
			}
		})
	}
	if *status != "" {
		run, err := app.client.GetRun(ctx, *status)
		if err != nil {
			return err
		}
		return printRun(app, run, *exportOut)
	}
	spec, err := requireArg(rest, 0, "run-spec path (YAML)")
	if err != nil {
		return err
	}
	data, err := os.ReadFile(spec)
	if err != nil {
		return err
	}
	parsed, err := runspec.Parse(data)
	if err != nil {
		return err
	}
	if *agentID == "" {
		*agentID = os.Getenv("USER")
		if *agentID == "" {
			*agentID = "manzanas-cli"
		}
	}
	run, err := app.client.StartRun(ctx, proto.RunRequest{
		Spec: parsed, AgentID: *agentID, Async: *async,
	})
	if err != nil {
		return err
	}
	return printRun(app, run, *exportOut)
}

// printRun renders a run result (and writes its export when asked). A
// failed run's verdict is the returned error; an export problem is a
// stderr warning so it never masks the failure.
func printRun(app *appEnv, run proto.Run, exportOut string) error {
	if exportOut == "-" && app.json {
		return fmt.Errorf("run: --json cannot be combined with -o - (stdout carries the export markdown)")
	}
	var exportErr error
	if exportOut != "" {
		switch {
		case run.ExportMD != "":
			if exportOut == "-" {
				fmt.Fprint(app.stdout, run.ExportMD)
			} else if err := os.WriteFile(exportOut, []byte(run.ExportMD), 0o644); err != nil {
				return err
			}
		case run.State == proto.RunPassed || run.State == proto.RunFailed:
			exportErr = fmt.Errorf("run %s has no journal export to write to %s (journaling disabled or artifacts.export=false)", run.ID, exportOut)
		default:
			fmt.Fprintf(app.stderr, "run %s has no export yet; re-run `manzanas run --status %s -o %s` once it finishes\n", run.ID, run.ID, exportOut)
		}
	}
	if exportOut == "-" {
		// stdout carries the export markdown; keep the summary off it.
		runSummary(run)(app.stderr)
	} else if err := app.emit(run, runSummary(run)); err != nil {
		return err
	}
	if run.State == proto.RunFailed {
		if exportErr != nil {
			fmt.Fprintln(app.stderr, exportErr)
		}
		return fmt.Errorf("run %s failed", run.ID)
	}
	return exportErr
}

// runSummary renders the human-readable run result.
func runSummary(run proto.Run) func(w io.Writer) {
	return func(w io.Writer) {
		fmt.Fprintf(w, "run %s: %s\n", run.ID, run.State)
		if run.LeaseID != "" {
			fmt.Fprintf(w, "  journal run: %s\n", run.LeaseID)
		}
		if run.TargetUDID != "" {
			fmt.Fprintf(w, "  target: %s\n", run.TargetUDID)
		}
		for _, st := range run.Steps {
			line := fmt.Sprintf("  step %d %s: %s", st.Index, st.Action, st.Status)
			if st.Name != "" {
				line = fmt.Sprintf("  step %d %s (%s): %s", st.Index, st.Action, st.Name, st.Status)
			}
			if st.Error != nil {
				line += " — " + st.Error.Message
			}
			fmt.Fprintln(w, line)
		}
		if run.Error != nil {
			fmt.Fprintf(w, "  error: %s: %s\n", run.Error.Code, run.Error.Message)
		}
		if run.State == proto.RunPending || run.State == proto.RunRunning {
			fmt.Fprintf(w, "  poll: manzanas run --status %s\n", run.ID)
		}
	}
}
