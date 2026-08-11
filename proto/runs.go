package proto

import "time"

// This file defines the one-call run primitive (POST /v0/runs): a single
// request that executes the whole canonical loop — acquire a lease on a
// matching target, boot it, optionally apply fixtures, install and launch
// an app, execute a list of steps through the actions backend, capture
// artifacts into the run journal, and release the lease (applying its
// reset). See proto/PROTOCOL.md §9 and docs/runs.md.
//
// The same structs double as the declarative YAML run-spec consumed by
// `manzanas run spec.yaml` and the MCP `run` tool, hence the yaml tags.

// RunState is the lifecycle of a run resource.
type RunState string

const (
	RunPending RunState = "pending" // created, not yet executing
	RunRunning RunState = "running"
	RunPassed  RunState = "passed" // every stage and step succeeded
	RunFailed  RunState = "failed" // a stage or step failed (see Error/Steps)
)

// RunSpec declares everything a run needs: the target requirements, the
// app under test, the steps, artifact expectations, and timeouts.
type RunSpec struct {
	// Name labels the run in the journal and results (optional).
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
	// Target selects and prepares the simulator/device to lease.
	Target RunTarget `json:"target" yaml:"target"`
	// App optionally installs and/or launches an app before the steps.
	App *RunApp `json:"app,omitempty" yaml:"app,omitempty"`
	// Steps run in order through the actions backend (PROTOCOL.md §5).
	Steps []RunStep `json:"steps,omitempty" yaml:"steps,omitempty"`
	// Artifacts declares run-level artifact expectations.
	Artifacts RunArtifacts `json:"artifacts,omitempty" yaml:"artifacts,omitempty"`
	// Timeouts bound the run's stages; zero values use daemon defaults.
	Timeouts RunTimeouts `json:"timeouts,omitempty" yaml:"timeouts,omitempty"`
}

// RunTarget is the run's target requirement: which target to lease and
// how to prepare it.
type RunTarget struct {
	// Labels must all be present on the matched target (e.g. ["ios26"]).
	Labels []string `json:"labels,omitempty" yaml:"labels,omitempty"`
	// UDID pins the run to a specific target.
	UDID string `json:"udid,omitempty" yaml:"udid,omitempty"`
	// Runtime and DeviceType are convenience requirements translated to
	// the registry's derived label slugs (e.g. "iOS 26.5" -> ios26.5,
	// "iPhone 17 Pro" -> iphone-17-pro) and appended to Labels.
	Runtime    string `json:"runtime,omitempty" yaml:"runtime,omitempty"`
	DeviceType string `json:"device_type,omitempty" yaml:"device_type,omitempty"`
	// Image names a golden image the target must be stamped from.
	// Reserved: schema-compatible but not yet implemented — a run with a
	// non-empty image is refused with not_implemented (see docs/runs.md).
	Image string `json:"image,omitempty" yaml:"image,omitempty"`
	// Fixtures are applied in order after the target boots (state slice).
	Fixtures []RunFixture `json:"fixtures,omitempty" yaml:"fixtures,omitempty"`
	// Reset is the lease's auto-reset applied when the run releases it:
	// "none" (default), "erase", or "snapshot:<name>".
	Reset string `json:"reset,omitempty" yaml:"reset,omitempty"`
}

// RunFixture names one state fixture plus its payload (docs/state.md).
type RunFixture struct {
	Name    string         `json:"name" yaml:"name"`
	Payload map[string]any `json:"payload,omitempty" yaml:"payload,omitempty"`
}

// RunApp installs and/or launches the app under test.
type RunApp struct {
	// Path is a .app bundle on the daemon host to install (optional).
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
	// BundleID identifies the app to launch. Required when Launch is
	// true (the default); optional when only installing.
	BundleID string `json:"bundle_id,omitempty" yaml:"bundle_id,omitempty"`
	// Launch controls whether the app is launched after install;
	// defaults to true when BundleID is set.
	Launch *bool `json:"launch,omitempty" yaml:"launch,omitempty"`
	// TerminateRunning relaunches over a running instance.
	TerminateRunning bool `json:"terminate_running,omitempty" yaml:"terminate_running,omitempty"`
	// Args are extra launch arguments.
	Args []string `json:"args,omitempty" yaml:"args,omitempty"`
}

// RunStep is one native-DSL step: an action kind from PROTOCOL.md §5 plus
// its payload, dispatched exactly like POST /v0/actions (and journaled
// identically).
type RunStep struct {
	// Name labels the step in results and the journal (optional).
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
	// Action is the action kind (tap, type, tap_element, wait_for_element,
	// screenshot, audit, ...).
	Action string `json:"action,omitempty" yaml:"action,omitempty"`
	// With is the action payload, passed through verbatim.
	With map[string]any `json:"with,omitempty" yaml:"with,omitempty"`
	// TimeoutSeconds bounds this step; 0 uses timeouts.step_seconds.
	TimeoutSeconds int `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
	// ContinueOnError lets the run proceed past this step's failure
	// (the run still finishes failed).
	ContinueOnError bool `json:"continue_on_error,omitempty" yaml:"continue_on_error,omitempty"`
	// MaestroFlow names a Maestro flow file to execute against the
	// leased target. Reserved: schema-compatible but not yet implemented —
	// a run containing a maestro_flow step is refused with
	// not_implemented (see docs/runs.md).
	MaestroFlow string `json:"maestro_flow,omitempty" yaml:"maestro_flow,omitempty"`
}

// RunArtifacts declares run-level artifact expectations. Per-step evidence
// (screenshots, audit findings, a11y hashes, logs) is journaled by the
// action pipeline regardless.
type RunArtifacts struct {
	// FinalScreenshot captures a screenshot into the journal after the
	// last step (even when a step failed); defaults to true.
	FinalScreenshot *bool `json:"final_screenshot,omitempty" yaml:"final_screenshot,omitempty"`
	// Export includes the journal's markdown export in the finished run
	// resource (export_md); defaults to true.
	Export *bool `json:"export,omitempty" yaml:"export,omitempty"`
}

// RunTimeouts bounds the run's stages (seconds; zero = daemon default).
type RunTimeouts struct {
	// AcquireSeconds bounds waiting for a queued lease (default 60).
	AcquireSeconds int `json:"acquire_seconds,omitempty" yaml:"acquire_seconds,omitempty"`
	// RunSeconds bounds the whole run (default 600, max 3600).
	RunSeconds int `json:"run_seconds,omitempty" yaml:"run_seconds,omitempty"`
	// StepSeconds bounds each step without its own timeout (default 60).
	StepSeconds int `json:"step_seconds,omitempty" yaml:"step_seconds,omitempty"`
}

// RunRequest is the POST /v0/runs body.
type RunRequest struct {
	Spec RunSpec `json:"spec"`
	// AgentID attributes the run's lease and journal entries; session_id
	// is accepted as an alias, exactly like leases.acquire.
	AgentID   string `json:"agent_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Async returns 202 with the pending run immediately; poll
	// GET /v0/runs/{id}. Default (sync) blocks until the run finishes.
	Async bool `json:"async,omitempty"`
}

// NormalizeAgentID resolves the session_id alias, like leases.acquire.
func (r *RunRequest) NormalizeAgentID() {
	if r.AgentID == "" {
		r.AgentID = r.SessionID
	}
}

// RunStepStatus is the outcome of one executed step.
type RunStepStatus string

const (
	StepOK      RunStepStatus = "ok"
	StepError   RunStepStatus = "error"
	StepSkipped RunStepStatus = "skipped" // not run (earlier failure or timeout)
)

// RunStepResult reports one step's outcome.
type RunStepResult struct {
	Index      int           `json:"index"`
	Name       string        `json:"name,omitempty"`
	Action     string        `json:"action"`
	Status     RunStepStatus `json:"status"`
	Error      *Error        `json:"error,omitempty"`
	DurationMS int64         `json:"duration_ms,omitempty"`
	// JournalRef points at the step's journal entry when journaling is on.
	JournalRef *JournalRef `json:"journal_ref,omitempty"`
}

// Run is the run resource returned by POST /v0/runs and GET /v0/runs/{id}.
type Run struct {
	ID    string   `json:"id"`
	State RunState `json:"state"`
	Spec  RunSpec  `json:"spec"`
	// AgentID is the requester (mirrored onto the lease and journal).
	AgentID string `json:"agent_id,omitempty"`
	// LeaseID is the run's lease — and therefore its journal run ID.
	LeaseID string `json:"lease_id,omitempty"`
	// TargetUDID is the matched target once the lease is granted.
	TargetUDID string `json:"target_udid,omitempty"`
	// Stage is the stage the run is in (or failed in): "acquire", "boot",
	// "fixtures", "install", "launch", "steps", "artifacts", "release".
	Stage string          `json:"stage,omitempty"`
	Steps []RunStepResult `json:"steps,omitempty"`
	// Error is the failure that ended the run (stage failures; the first
	// failing step's error is also mirrored here).
	Error      *Error     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// ExportMD is the journal's markdown export, included on finished
	// runs when artifacts.export is true (the default) and journaling is
	// enabled.
	ExportMD string `json:"export_md,omitempty"`
	// Host and HostAddr identify the daemon executing this run. Set only
	// by a broker (like Lease.Host/HostAddr): clients can reach the run's
	// journal and target directly at HostAddr.
	Host     string `json:"host,omitempty"`
	HostAddr string `json:"host_addr,omitempty"`
}

// RunList is the GET /v0/runs response.
type RunList struct {
	Runs []Run `json:"runs"`
}
