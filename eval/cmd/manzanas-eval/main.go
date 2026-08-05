// Command manzanas-eval runs eval scenarios against a manzanasd daemon and
// writes a markdown + JSON report.
//
//	manzanas-eval --daemon http://mac-host:7433 --runs 3 --out eval-out \
//	    eval/scenarios/*.yaml
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/BariBariGood/manzanas/eval"
)

func main() {
	daemon := flag.String("daemon", "http://localhost:7433", "manzanasd base URL")
	runs := flag.Int("runs", 3, "runs per scenario (fresh lease + reset each run)")
	out := flag.String("out", "eval-out", "output directory for reports and artifacts")
	agentID := flag.String("agent-id", "manzanas-eval", "agent_id used for leases")
	udid := flag.String("udid", "", "pin every lease to this target UDID (overrides scenario lease.udid)")
	profile := flag.String("profile", "m3", "host-class timing profile: m3 or intel")
	timeoutScale := flag.Float64("timeout-scale", 0, "override the profile's step-timeout multiplier")
	waitScale := flag.Float64("wait-scale", 0, "override the profile's wait_* payload multiplier (timeout_ms/interval_ms)")
	overloadBudget := flag.Duration("overload-budget", 2*time.Minute, "total time to back off and retry boots the daemon refuses with 503 overloaded (0 = fail fast)")
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: manzanas-eval [flags] scenario.yaml [scenario.yaml ...]")
		flag.PrintDefaults()
		os.Exit(2)
	}
	prof, err := eval.ProfileByName(*profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "manzanas-eval:", err)
		os.Exit(2)
	}
	if *timeoutScale > 0 {
		prof.TimeoutScale = *timeoutScale
	}
	if *waitScale > 0 {
		prof.WaitScale = *waitScale
	}
	if err := run(*daemon, *runs, *out, *agentID, *udid, prof, *overloadBudget, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "manzanas-eval:", err)
		os.Exit(1)
	}
}

func run(daemon string, runs int, out, agentID, udid string, prof eval.TimingProfile, overloadBudget time.Duration, paths []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if runs <= 0 {
		runs = 3 // keep in sync with the runner's default
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	var scenarios []*eval.Scenario
	for _, path := range paths {
		s, err := eval.LoadScenario(path)
		if err != nil {
			return err
		}
		scenarios = append(scenarios, s)
	}

	client := eval.NewClient(daemon)
	client.OverloadBudget = overloadBudget
	if err := client.Healthz(ctx); err != nil {
		return fmt.Errorf("daemon %s not healthy: %w", daemon, err)
	}

	runner := eval.NewRunner(client, eval.RunnerConfig{
		Runs:        runs,
		AgentID:     agentID,
		ArtifactDir: out,
		Log:         os.Stderr,
		TargetUDID:  udid,
		Profile:     prof,
	})
	if prof.TimeoutScale != 1 || prof.WaitScale != 1 {
		fmt.Fprintf(os.Stderr, "timing profile %s: timeout x%.2g, wait_* x%.2g\n", prof.Name, prof.TimeoutScale, prof.WaitScale)
	}

	// Even when a scenario errors (e.g. Ctrl-C between runs), write a
	// report for whatever results were collected before surfacing the
	// error.
	var runErr error
	var results []*eval.ScenarioResult
	for _, s := range scenarios {
		sr, err := runner.RunScenario(ctx, s)
		if sr != nil {
			results = append(results, sr)
		}
		if err != nil {
			runErr = err
			break
		}
	}

	report := eval.BuildReport(daemon, runs, results)
	md := report.Markdown()
	if err := os.WriteFile(filepath.Join(out, "report.md"), []byte(md), 0o644); err != nil {
		return err
	}
	raw, err := report.JSON()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "report.json"), raw, 0o644); err != nil {
		return err
	}
	fmt.Print(md)

	if runErr != nil {
		return runErr
	}
	for _, s := range report.Scenarios {
		if s.Passed != s.Runs {
			return fmt.Errorf("scenario %q: %d/%d runs passed", s.Name, s.Passed, s.Runs)
		}
		if s.HashConsistent != nil && !*s.HashConsistent {
			return fmt.Errorf("scenario %q: saved tree hashes drifted between runs", s.Name)
		}
	}
	return nil
}
