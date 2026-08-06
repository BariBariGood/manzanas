// Package proto defines the versioned wire types for the manzanasd protocol.
//
// Protocol version: v0. See PROTOCOL.md for the full specification.
// All other manzanasd packages (and external clients) import these types;
// keep changes backward-compatible within a major version.
package proto

import (
	"encoding/json"
	"fmt"
	"time"
)

// Version is the current protocol version string.
const Version = "v0"

// TargetKind distinguishes simulator targets from physical devices (v0.2+).
type TargetKind string

const (
	TargetSimulator TargetKind = "simulator"
	TargetDevice    TargetKind = "device"
)

// TargetState is the lifecycle state of a target as reported by the registry.
type TargetState string

const (
	StateShutdown     TargetState = "Shutdown"
	StateBooted       TargetState = "Booted"
	StateBooting      TargetState = "Booting"
	StateShuttingDown TargetState = "ShuttingDown"
	StateUnknown      TargetState = "Unknown"
)

// Target describes one leasable target (simulator or, later, device).
type Target struct {
	UDID       string      `json:"udid"`
	Kind       TargetKind  `json:"kind"`
	Name       string      `json:"name"`
	Runtime    string      `json:"runtime"`     // e.g. "iOS 26.5"
	DeviceType string      `json:"device_type"` // e.g. "iPhone 17 Pro"
	State      TargetState `json:"state"`
	Labels     []string    `json:"labels"` // e.g. ["ios26", "iphone-17-pro", "simulator"]
	// Host is the fleet host that owns this target. Set only by a broker
	// federating multiple daemons; a single daemon leaves it empty.
	Host string `json:"host,omitempty"`
	// Warm reports that the target is parked in the daemon's warm pool
	// (booted, SIGSTOPped, thawed in ~26ms on grant). Only daemons with a
	// warm pool set it; schedulers may prefer warm targets over cold boots.
	Warm bool `json:"warm,omitempty"`
	// Recording reports that a video recording is currently live on the
	// target. Only daemons with recording wired set it.
	Recording bool `json:"recording,omitempty"`
}

// HostCapacity is a host's simulator capacity class as reported by
// GET /v0/status. Zero values mean "uncapped" (no warm pool configured).
type HostCapacity struct {
	MaxBootedRunning   int `json:"max_booted_running"`
	MaxParked          int `json:"max_parked"`
	MaxConcurrentBoots int `json:"max_concurrent_boots"`
}

// HostGates reports the host safety gates as evaluated with the daemon's
// current thresholds. A false gate means the daemon would refuse a cold
// simulator boot right now.
type HostGates struct {
	LoadOK bool `json:"load_ok"`
	DiskOK bool `json:"disk_ok"`
}

// HostStatus is one daemon's load/occupancy snapshot, served at
// GET /v0/status. All fields are additive; schedulers must tolerate
// daemons that do not serve the endpoint at all (404).
type HostStatus struct {
	Capacity HostCapacity `json:"capacity"`
	// Running counts Booted, un-parked simulators.
	Running int `json:"running"`
	// UnmanagedSims counts Booted, un-parked simulators the daemon does
	// not manage (not pool members and not booted through the daemon):
	// typically another agent's simctl-created sims. The daemon's janitor
	// leaves these alone unless stale-lock reaping is opted in.
	UnmanagedSims int `json:"unmanaged_sims,omitempty"`
	// ReapedSims counts unmanaged sims the janitor has shut down because
	// their fleet lock file was stale or missing (opt-in stale-lock
	// reaping; sims are only ever shut down, never deleted).
	ReapedSims int `json:"reaped_sims,omitempty"`
	// Parked counts simulators held parked (SIGSTOPped) in the warm pool.
	Parked int `json:"parked"`
	// BootsInFlight counts boot slots currently held.
	BootsInFlight int `json:"boots_in_flight"`
	// LeasesActive and LeasesQueued are the lease manager's live counts,
	// including leases acquired directly on the daemon.
	LeasesActive  int       `json:"leases_active"`
	LeasesQueued  int       `json:"leases_queued"`
	LoadAvg1      float64   `json:"load_avg1"`
	CPUs          int       `json:"cpus"`
	FreeDiskBytes uint64    `json:"free_disk_bytes"`
	Gates         HostGates `json:"gates"`
	// PoolAdvice is the most recent advisory the daemon accepted via
	// POST /v0/pool/advise; nil when none has been received.
	PoolAdvice *PoolAdviceState `json:"pool_advice,omitempty"`
}

// PoolAdviceAction is a broker's advisory direction for a daemon's warm
// pool: grow (demand for a class keeps falling to cold boots) or shrink
// (warm capacity sat idle through the observation window).
type PoolAdviceAction string

const (
	AdviceGrow   PoolAdviceAction = "grow"
	AdviceShrink PoolAdviceAction = "shrink"
)

// PoolClassAdvice is one class's advisory entry. Labels identify the
// demand class (the requested label set); empty labels on a shrink mean
// the advice applies to the pool as a whole.
type PoolClassAdvice struct {
	Labels []string         `json:"labels,omitempty"`
	Action PoolAdviceAction `json:"action"`
	// ColdPlacements and WarmHits are the broker's windowed counts that
	// motivated the advice.
	ColdPlacements int    `json:"cold_placements,omitempty"`
	WarmHits       int    `json:"warm_hits,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// PoolAdviseRequest is the body of POST /v0/pool/advise: a scheduler's
// advisory view of warm-pool demand. Advice is never binding — the daemon
// records it and always retains final say via its own capacity class and
// safety gates.
type PoolAdviseRequest struct {
	// Source identifies the advising scheduler (e.g. "broker").
	Source        string            `json:"source,omitempty"`
	WindowSeconds int               `json:"window_seconds,omitempty"`
	Classes       []PoolClassAdvice `json:"classes"`
}

// PoolAdviceState is the daemon's record of the most recent advice it
// accepted, surfaced additively on HostStatus as pool_advice.
type PoolAdviceState struct {
	Source        string            `json:"source,omitempty"`
	ReceivedAt    time.Time         `json:"received_at"`
	WindowSeconds int               `json:"window_seconds,omitempty"`
	Classes       []PoolClassAdvice `json:"classes"`
}

// LeaseState is the lifecycle state of a lease.
type LeaseState string

const (
	LeaseActive   LeaseState = "active"
	LeaseQueued   LeaseState = "queued"
	LeaseExpired  LeaseState = "expired"
	LeaseReleased LeaseState = "released"
)

// Lease is a time-bounded exclusive claim on a target.
type Lease struct {
	ID         string     `json:"id"`
	TargetUDID string     `json:"target_udid,omitempty"` // empty while queued
	Labels     []string   `json:"labels"`
	State      LeaseState `json:"state"`
	AgentID    string     `json:"agent_id"`
	Purpose    string     `json:"purpose,omitempty"`
	TTLSeconds int        `json:"ttl_seconds"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"` // nil while queued
	// GraceUntil is set once an active lease passes its nominal
	// expires_at while the daemon's renewal grace window is still open:
	// the lease stays active (and renewable) until this deadline, after
	// which it expires and its reset/reclaim runs. Additive (v0).
	GraceUntil *time.Time `json:"grace_until,omitempty"`
	// ExpiresInSeconds is the derived time remaining until expires_at,
	// stamped on active leases by read endpoints (negative inside the
	// grace window). Additive (v0).
	ExpiresInSeconds *int      `json:"expires_in_seconds,omitempty"`
	QueuePosition    int       `json:"queue_position,omitempty"` // 1-based; 0 when active
	CreatedAt        time.Time `json:"created_at"`
	// Reset is the auto-reset applied when the lease ends:
	// "none" (default), "erase", or "snapshot:<name>".
	Reset string `json:"reset,omitempty"`
	// RequestedUDID is the specific target the acquire request pinned
	// itself to, if any; matching and queue promotion honor it.
	RequestedUDID string `json:"requested_udid,omitempty"`
	// Record is the auto-record mode requested at acquire time: "" (none)
	// or "video" (start a screen recording when the target is booted under
	// this lease; it is stopped and ingested when the lease ends).
	Record string `json:"record,omitempty"`
	// Host and HostAddr identify the daemon that owns this lease. Set only
	// by a broker: clients should talk to HostAddr directly for target ops
	// (boot, actions, streams) after acquiring through the broker.
	Host     string `json:"host,omitempty"`
	HostAddr string `json:"host_addr,omitempty"`
}

// AcquireLeaseRequest asks for a lease on any target matching all Labels.
// UDID, when set, pins the request to that specific target (which must
// also match Labels, if any are given).
type AcquireLeaseRequest struct {
	Labels  []string `json:"labels"`
	UDID    string   `json:"udid,omitempty"`
	AgentID string   `json:"agent_id"`
	// SessionID is an accepted alias for agent_id (agents often identify
	// themselves by a session ID). When agent_id is empty the alias fills
	// it; agent_id stays canonical and wins when both are set. The Lease
	// always carries the resolved value as agent_id.
	SessionID  string `json:"session_id,omitempty"`
	Purpose    string `json:"purpose,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"` // default 300, max 3600
	// Reset requests an auto-reset of the target when the lease is released
	// or expires: "none" (default), "erase", or "snapshot:<name>".
	Reset string `json:"reset,omitempty"`
	// Record requests auto-recording for the lease: "" (none) or "video"
	// (recording starts once the target is booted under the lease and is
	// stopped + ingested into the journal when the lease ends).
	Record string `json:"record,omitempty"`
}

// NormalizeAgentID resolves the session_id alias into AgentID: when
// agent_id is empty, session_id fills it. Servers call this once after
// decoding so everything downstream sees the canonical field only.
func (r *AcquireLeaseRequest) NormalizeAgentID() {
	if r.AgentID == "" {
		r.AgentID = r.SessionID
	}
}

// RecordingRequest starts a screen recording on a leased, booted target
// (POST /v0/targets/{udid}/recording).
type RecordingRequest struct {
	LeaseID string `json:"lease_id"`
	// Codec is "hevc" (default) or "h264".
	Codec string `json:"codec,omitempty"`
	// MaxSeconds caps the recording duration; 0 means the daemon default.
	// Values above the daemon cap are clamped down.
	MaxSeconds int `json:"max_seconds,omitempty"`
}

// Recording describes a live recording (start response).
type Recording struct {
	ID         string    `json:"recording_id"`
	TargetUDID string    `json:"udid"`
	Codec      string    `json:"codec"`
	MaxSeconds int       `json:"max_seconds"`
	StartedAt  time.Time `json:"started_at"`
}

// RecordingStopRequest stops a target's recording
// (POST /v0/targets/{udid}/recording/stop).
type RecordingStopRequest struct {
	LeaseID string `json:"lease_id"`
}

// RecordingArtifact locates the finalized video in the run's journal
// (mirrors the journal's artifact ref shape).
type RecordingArtifact struct {
	Path   string `json:"path"` // run-relative, artifacts/<sha256>.mp4
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// RecordingStopResult is the stop response: where the video landed and the
// finalized clip's stats.
type RecordingStopResult struct {
	OK          bool               `json:"ok"`
	RecordingID string             `json:"recording_id"`
	Artifact    *RecordingArtifact `json:"artifact,omitempty"`
	JournalRef  *JournalRef        `json:"journal_ref,omitempty"`
	Codec       string             `json:"codec"`
	DurationS   float64            `json:"duration_s"`
	Bytes       int64              `json:"bytes"`
	// Reason is why the recording ended: stopped, lease_end,
	// target_shutdown, max_seconds, max_bytes, daemon_restart,
	// daemon_shutdown, or exited.
	Reason string `json:"reason"`
}

// RenewLeaseRequest extends an active lease's TTL.
type RenewLeaseRequest struct {
	TTLSeconds int `json:"ttl_seconds,omitempty"` // default: original TTL
}

// ActionRequest dispatches an opaque action payload to the actions backend
// for the leased target. The payload schema is owned by the actions slice.
type ActionRequest struct {
	LeaseID string         `json:"lease_id"`
	Kind    string         `json:"kind"` // e.g. "tap", "swipe", "type", "observe"
	Payload map[string]any `json:"payload,omitempty"`
}

// ActionResult is the actions backend's opaque response.
type ActionResult struct {
	OK         bool           `json:"ok"`
	Result     map[string]any `json:"result,omitempty"`
	JournalRef *JournalRef    `json:"journal_ref,omitempty"`
}

// BatchAction is one entry in a batch dispatch: the same kind/payload
// pair as ActionRequest, sharing the batch's lease.
type BatchAction struct {
	Kind    string         `json:"kind"`
	Payload map[string]any `json:"payload,omitempty"`
}

// UnmarshalJSON rejects unknown top-level keys in a batch entry. A
// misspelled field (e.g. "params" instead of "payload") would otherwise be
// silently dropped and the action dispatched with defaults.
func (a *BatchAction) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for k := range raw {
		if k != "kind" && k != "payload" {
			return fmt.Errorf("unknown field %q in batch action entry (allowed: \"kind\", \"payload\")", k)
		}
	}
	type plain BatchAction
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*a = BatchAction(v)
	return nil
}

// BatchActionRequest dispatches an ordered list of actions against the
// leased target in one round-trip. With StopOnError the batch halts at
// the first failing action; otherwise every action runs and failures are
// reported per entry.
type BatchActionRequest struct {
	LeaseID     string        `json:"lease_id"`
	StopOnError bool          `json:"stop_on_error,omitempty"`
	Actions     []BatchAction `json:"actions"`
}

// BatchItemResult reports one batch entry's outcome: exactly one of
// Result/Error is meaningful, keyed by OK.
type BatchItemResult struct {
	OK         bool           `json:"ok"`
	Result     map[string]any `json:"result,omitempty"`
	Error      *Error         `json:"error,omitempty"`
	JournalRef *JournalRef    `json:"journal_ref,omitempty"`
}

// BatchActionResult is the whole batch's response. OK is true only when
// every action succeeded; Completed counts the actions that ran (equal to
// len(Results); shorter than the request when StopOnError halted it).
type BatchActionResult struct {
	OK        bool              `json:"ok"`
	Results   []BatchItemResult `json:"results"`
	Completed int               `json:"completed"`
}

// StreamRequest negotiates a media stream for a target. Viewing is
// read-only, so no lease is required: identify the target either by UDID
// directly or via an active lease ID (in which case the lease's target is
// streamed).
type StreamRequest struct {
	UDID    string `json:"udid,omitempty"`
	LeaseID string `json:"lease_id,omitempty"`
	Format  string `json:"format,omitempty"` // "mjpeg" (default) | "h264" (reserved)
	MaxFPS  int    `json:"max_fps,omitempty"`
	// MaxDim caps the longer frame edge in pixels (frames are downscaled
	// server-side, preserving aspect ratio). 0 keeps the native size.
	MaxDim int `json:"max_dim,omitempty"`
	// Quality is the JPEG re-encode quality (1-100). 0 keeps the capture
	// bytes untouched unless MaxDim forces a re-encode (then a default
	// quality is used).
	Quality int `json:"quality,omitempty"`
}

// StreamOffer is the server's answer to a StreamRequest.
type StreamOffer struct {
	StreamID string `json:"stream_id"`
	Format   string `json:"format"`
	URL      string `json:"url"`       // WS endpoint carrying the media frames
	MJPEGURL string `json:"mjpeg_url"` // HTTP multipart/x-mixed-replace endpoint
	ViewURL  string `json:"view_url"`  // browser view page for the target
	FPS      int    `json:"fps"`       // effective capture rate after clamping
	// MaxDim and Quality echo the effective frame transform, if any.
	MaxDim  int `json:"max_dim,omitempty"`
	Quality int `json:"quality,omitempty"`
	// Holder is the target's current active lease, if any. Streams don't
	// require a lease, but viewers get to see who is driving the target.
	Holder *Lease `json:"holder,omitempty"`
}

// SnapshotInfo describes one stored simulator snapshot.
type SnapshotInfo struct {
	ID         string    `json:"id"`
	SourceUDID string    `json:"source_udid"`
	CloneUDID  string    `json:"clone_udid,omitempty"` // simctl clone backing the snapshot
	Label      string    `json:"label,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// SnapshotDeviceNamePrefix names the hidden simctl clone devices that back
// snapshots; the registry excludes them from target enumeration.
const SnapshotDeviceNamePrefix = "manzanasd-snap-"

// SnapshotRequest captures a snapshot of the leased target.
type SnapshotRequest struct {
	LeaseID string `json:"lease_id"`
	Label   string `json:"label,omitempty"`
}

// RestoreRequest restores the leased target to a snapshot (by ID or label).
// If the target is booted, Reboot=true performs shutdown+restore+boot;
// otherwise the request fails with "target_busy".
type RestoreRequest struct {
	LeaseID  string `json:"lease_id"`
	Snapshot string `json:"snapshot"`
	Reboot   bool   `json:"reboot,omitempty"`
}

// RestoreResult reports a completed restore.
type RestoreResult struct {
	OK       bool   `json:"ok"`
	Snapshot string `json:"snapshot"`
	// Rebooted is true when the target was booted and the engine performed
	// the shutdown+restore+boot cycle.
	Rebooted bool `json:"rebooted"`
}

// EraseRequest factory-resets the leased target (must be shutdown).
type EraseRequest struct {
	LeaseID string `json:"lease_id"`
}

// FixtureRequest applies a named fixture to the leased target. The payload
// schema is owned by the state slice (see docs/state.md).
type FixtureRequest struct {
	LeaseID string         `json:"lease_id"`
	Name    string         `json:"name"`
	Payload map[string]any `json:"payload,omitempty"`
}

// ImageInfo describes one stored golden image: an archived, optionally
// slimmed simulator data directory that can be stamped out into fresh
// simulators in seconds.
type ImageInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	DeviceType  string `json:"device_type"` // e.g. "iPhone 17"
	Runtime     string `json:"runtime"`     // e.g. "iOS 26.5"
	SlimProfile string `json:"slim_profile,omitempty"`
	// DisabledServices is the launchctl disable set captured from the
	// booted builder right after slimming. launchctl disables are keyed
	// to the sim UDID outside the data directory (iOS 18+), so a stamped
	// sim keeps none of them — the stamp flow re-applies this exact set
	// to every sim it creates and verifies it took.
	DisabledServices []string `json:"disabled_services,omitempty"`
	// DisabledCount is len(DisabledServices), recorded for capacity
	// planning without shipping the full list everywhere.
	DisabledCount int `json:"disabled_count,omitempty"`
	// PostSlimProcs is the running launchd-service count measured on the
	// booted builder after slimming (0 when unmeasured, -1 on failure).
	PostSlimProcs int       `json:"post_slim_procs,omitempty"`
	SizeBytes     int64     `json:"size_bytes"`
	SHA256        string    `json:"sha256,omitempty"` // hex digest of the archive, checked before every stamp
	CreatedAt     time.Time `json:"created_at"`
}

// ImageDeviceNamePrefix names the transient builder simulators created by
// golden-image builds; the registry excludes them from target enumeration.
const ImageDeviceNamePrefix = "manzanasd-img-"

// ImageBuildRequest builds a golden image: the daemon creates a fresh
// simulator of the given device type + runtime, optionally slims it with
// the named simslim profile, shuts it down, archives its data directory,
// and deletes the builder sim.
type ImageBuildRequest struct {
	DeviceType  string `json:"device_type"` // simctl devicetype name or identifier
	Runtime     string `json:"runtime"`     // simctl runtime name or identifier
	Name        string `json:"name,omitempty"`
	SlimProfile string `json:"slim_profile,omitempty"` // requires simslim on the host
}

// ImageStampRequest stamps out Count fresh simulators from a golden image.
// Each is named "<name_prefix>-<n>" and appears in the registry as a
// leasable target.
type ImageStampRequest struct {
	Count      int    `json:"count"`
	NamePrefix string `json:"name_prefix,omitempty"` // default "manzanas"
}

// StampedSim identifies one simulator created by a stamp operation.
type StampedSim struct {
	UDID string `json:"udid"`
	Name string `json:"name"`
}

// ImageStampResult reports a completed stamp operation.
type ImageStampResult struct {
	OK         bool         `json:"ok"`
	ImageID    string       `json:"image_id"`
	Created    []StampedSim `json:"created"`
	DurationMS int64        `json:"duration_ms"`
}

// JournalRef points at a run-journal entry or segment.
type JournalRef struct {
	RunID string `json:"run_id"`
	Seq   int64  `json:"seq"`
}

// Error is the wire error shape used by both HTTP and WS surfaces.
type Error struct {
	Code    string `json:"code"` // machine-readable, e.g. "not_found", "not_implemented"
	Message string `json:"message"`
	// Detail carries actionable context for the error when the daemon
	// has any — e.g. a target_not_booted error references the daemon's
	// record of who shut the target down and why. Additive (v0).
	Detail string `json:"detail,omitempty"`
	// RetryAfterSeconds is a machine-readable retry hint on transient
	// refusals (code "overloaded"); HTTP responses mirror it in a
	// Retry-After header. Additive (v0).
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

// Well-known error codes.
const (
	ErrNotFound        = "not_found"
	ErrNotImplemented  = "not_implemented"
	ErrBadRequest      = "bad_request"
	ErrLeaseExpired    = "lease_expired"
	ErrNoMatch         = "no_match"          // no target matches the requested labels
	ErrStreamLimit     = "stream_limit"      // the streamer is at its configured capacity
	ErrViewerLimit     = "viewer_limit"      // the stream is at its configured viewer capacity
	ErrStreamGone      = "stream_gone"       // the stream was closed or reaped; re-open to get a fresh one
	ErrOverloaded      = "overloaded"        // a per-connection/host concurrency limit was hit; back off and retry
	ErrTargetBusy      = "target_busy"       // target is in the wrong lifecycle state for this op
	ErrTargetNotBooted = "target_not_booted" // action requires a booted target but it is shut down
	ErrUnavailable     = "unavailable"       // a required host tool (e.g. AXe) is missing
	ErrTimeout         = "timeout"           // a wait_* action's budget was exhausted
	ErrOffViewport     = "off_viewport"      // an element action's tap point lies outside the device viewport
	ErrFocusRequired   = "focus_required"    // a typing action with require_focus found no focused text field (no on-screen keyboard)
	ErrReadOnly        = "read_only"         // dashboard controls disabled (--dash-readonly)
	ErrInternal        = "internal"
)
