package eval

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// RunnerConfig configures a benchmark run.
type RunnerConfig struct {
	// Runs is the number of times each scenario is executed (fresh lease +
	// reset per run). Default 3.
	Runs int
	// AgentID identifies the harness to the daemon's lease table.
	AgentID string
	// ArtifactDir receives screenshot artifacts; empty = don't save.
	ArtifactDir string
	// Log receives progress lines; nil = silent.
	Log io.Writer
	// TargetUDID, when set, pins every lease to that target, overriding
	// any per-scenario lease.udid.
	TargetUDID string
	// Profile adapts step timeouts and wait_* polling to the host class
	// (zero value = identity, i.e. the M3 tuning as written).
	Profile TimingProfile
}

// StepResult is one step execution within one run.
type StepResult struct {
	Step      string        `json:"step"`
	Op        string        `json:"op"`
	OK        bool          `json:"ok"`
	Detail    string        `json:"detail,omitempty"`
	Error     string        `json:"error,omitempty"`
	LatencyMS float64       `json:"latency_ms"`
	Latency   time.Duration `json:"-"`
}

// RunResult is one full scenario execution.
type RunResult struct {
	Run         int               `json:"run"` // 1-based
	OK          bool              `json:"ok"`
	Error       string            `json:"error,omitempty"` // setup/teardown error, if any
	TargetUDID  string            `json:"target_udid,omitempty"`
	LeaseID     string            `json:"lease_id,omitempty"`
	Steps       []StepResult      `json:"steps"`
	SavedHashes map[string]string `json:"saved_hashes,omitempty"`
	Artifacts   []string          `json:"artifacts,omitempty"`
	Duration    time.Duration     `json:"-"`
	DurationMS  float64           `json:"duration_ms"`
	StartedAt   time.Time         `json:"started_at"`
}

// ScenarioResult aggregates all runs of one scenario.
type ScenarioResult struct {
	Scenario  *Scenario   `json:"scenario"`
	Runs      []RunResult `json:"runs"`
	StartedAt time.Time   `json:"started_at"`
}

// Runner executes scenarios against a daemon.
type Runner struct {
	client *Client
	cfg    RunnerConfig
	logMu  sync.Mutex // guards cfg.Log (written from keepalive too)
}

// NewRunner returns a runner for the given client.
func NewRunner(client *Client, cfg RunnerConfig) *Runner {
	if cfg.Runs <= 0 {
		cfg.Runs = 3
	}
	if cfg.AgentID == "" {
		cfg.AgentID = "manzanas-eval"
	}
	return &Runner{client: client, cfg: cfg}
}

func (r *Runner) logf(format string, args ...any) {
	if r.cfg.Log == nil {
		return
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	fmt.Fprintf(r.cfg.Log, format+"\n", args...)
}

// RunScenario executes the scenario cfg.Runs times and returns the
// aggregated result. Run failures do not abort remaining runs.
func (r *Runner) RunScenario(ctx context.Context, s *Scenario) (*ScenarioResult, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	result := &ScenarioResult{Scenario: s, StartedAt: time.Now().UTC()}
	for run := 1; run <= r.cfg.Runs; run++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		r.logf("scenario %q run %d/%d", s.Name, run, r.cfg.Runs)
		rr := r.runOnce(ctx, s, run)
		result.Runs = append(result.Runs, rr)
		status := "PASS"
		if !rr.OK {
			status = "FAIL"
		}
		r.logf("scenario %q run %d/%d: %s (%.1fs)", s.Name, run, r.cfg.Runs, status, rr.Duration.Seconds())
	}
	return result, nil
}

func (r *Runner) runOnce(ctx context.Context, s *Scenario, run int) (rr RunResult) {
	rr = RunResult{Run: run, StartedAt: time.Now().UTC()}
	defer func() {
		rr.Duration = time.Since(rr.StartedAt)
		rr.DurationMS = float64(rr.Duration.Milliseconds())
	}()

	acquireTimeout := s.Lease.AcquireTimeout.Std()
	if acquireTimeout <= 0 {
		acquireTimeout = 5 * time.Minute
	}
	acquireTimeout = r.cfg.Profile.scaleTimeout(acquireTimeout)
	pinUDID := s.Lease.UDID
	if r.cfg.TargetUDID != "" {
		pinUDID = r.cfg.TargetUDID
	}
	acqCtx, cancel := context.WithTimeout(ctx, acquireTimeout)
	lease, err := r.client.AcquireLease(acqCtx, proto.AcquireLeaseRequest{
		Labels:     s.Lease.Labels,
		UDID:       pinUDID,
		AgentID:    r.cfg.AgentID,
		Purpose:    "eval:" + s.Name,
		TTLSeconds: s.Lease.TTLSeconds,
		Reset:      s.Lease.Reset,
	})
	cancel()
	if err != nil {
		rr.Error = "acquire lease: " + err.Error()
		return rr
	}
	rr.LeaseID = lease.ID
	rr.TargetUDID = lease.TargetUDID
	if pinUDID != "" && lease.TargetUDID != pinUDID {
		rr.Error = fmt.Sprintf("lease %s granted target %s, not the pinned %s", lease.ID, lease.TargetUDID, pinUDID)
		relCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = r.client.ReleaseLease(relCtx, lease.ID)
		return rr
	}
	defer func() {
		relCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := r.client.ReleaseLease(relCtx, lease.ID); err != nil && rr.Error == "" {
			rr.Error = "release lease: " + err.Error()
			rr.OK = false
		}
	}()

	// Keep the lease alive for the duration of the run; scenarios can
	// outlive the daemon's TTL otherwise.
	keepaliveDone := make(chan struct{})
	defer close(keepaliveDone)
	go r.keepalive(lease, keepaliveDone)

	rc := &runContext{
		client:   r.client,
		scenario: s.Name,
		lease:    lease.ID,
		udid:     lease.TargetUDID,
		run:      run,
		saved:    map[string]string{},
		artDir:   r.cfg.ArtifactDir,
		profile:  r.cfg.Profile,
	}
	// Snapshots taken during a run are backed by real simulator clones on
	// the host; delete them before the lease is released so repeated
	// benchmark runs don't accumulate orphaned devices.
	defer func() {
		r.cleanupSnapshots(s, rc, lease.ID)
	}()

	defaultTimeout := s.DefaultTimeout.Std()
	if defaultTimeout <= 0 {
		defaultTimeout = 60 * time.Second
	}
	defaultTimeout = r.cfg.Profile.scaleTimeout(defaultTimeout)

	allOK := true
	for i := range s.Steps {
		st := &s.Steps[i]
		sr := StepResult{Step: st.Label(i), Op: st.Op}
		timeout := r.cfg.Profile.scaleTimeout(st.Timeout.Std())
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		// A wait step's sleep must fit in the budget regardless of where
		// it came from: a sub-1 timeout scale can shrink an explicit
		// timeout below the wait's duration even though validation
		// rejected one written that short.
		if st.Op == "wait" && timeout < st.Duration.Std()+time.Second {
			timeout = st.Duration.Std() + time.Second
		}
		stepCtx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		ex, err := executorFor(st.Op)
		var detail string
		if err == nil {
			detail, err = ex.Execute(stepCtx, rc, st)
		}
		cancel()
		sr.Latency = time.Since(start)
		sr.LatencyMS = float64(sr.Latency.Microseconds()) / 1000
		sr.Detail = detail
		if err != nil {
			sr.Error = err.Error()
			allOK = false
			rr.Steps = append(rr.Steps, sr)
			r.logf("  step %s: FAIL: %v", sr.Step, err)
			break // remaining steps depend on this one; stop the run
		}
		sr.OK = true
		rr.Steps = append(rr.Steps, sr)
		r.logf("  step %s: ok (%.0fms) %s", sr.Step, sr.LatencyMS, detail)
	}
	rr.SavedHashes = rc.saved
	rr.Artifacts = rc.artifacts
	rr.OK = allOK
	return rr
}

// cleanupSnapshots deletes the snapshots this run created. The IDs
// recorded from successful captures are augmented with the daemon's
// lease-scoped snapshot listing, matched by the scenario's snapshot
// labels: an interrupted capture can complete on the host after the
// client gave up, so its ID is never recorded. Only this run's labels
// are matched — foreign snapshots on the same target (e.g. a
// "snapshot:<name>" reset baseline) are left alone.
func (r *Runner) cleanupSnapshots(s *Scenario, rc *runContext, leaseID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	toDelete := map[string]bool{}
	for _, id := range rc.snapshots {
		toDelete[id] = true
	}
	labels := map[string]bool{}
	for i := range s.Steps {
		if s.Steps[i].Op == "snapshot" {
			labels[s.Steps[i].SnapshotLabel] = true
		}
	}
	if len(labels) > 0 {
		if snaps, err := r.client.ListSnapshots(ctx, leaseID); err != nil {
			r.logf("  cleanup: list snapshots: %v", err)
		} else {
			for _, sn := range snaps {
				if labels[sn.Label] {
					toDelete[sn.ID] = true
				}
			}
		}
	}
	for id := range toDelete {
		if err := r.client.DeleteSnapshot(ctx, id, leaseID); err != nil {
			r.logf("  cleanup: delete snapshot %s: %v", id, err)
		}
	}
}

// keepalive renews the lease at a third of its TTL until done is closed.
func (r *Runner) keepalive(lease *proto.Lease, done <-chan struct{}) {
	interval := time.Duration(lease.TTLSeconds) * time.Second / 3
	if interval <= 0 || interval > time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if _, err := r.client.RenewLease(ctx, lease.ID, 0); err != nil {
				r.logf("  keepalive: renew lease %s: %v", lease.ID, err)
			}
			cancel()
		}
	}
}
