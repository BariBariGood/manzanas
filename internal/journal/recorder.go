package journal

import (
	"context"

	"github.com/BariBariGood/manzanas/proto"
)

// Recorder is the thin middleware the protocol layer uses to journal every
// action generically, without coupling to any backend. A nil *Recorder is a
// no-op, so slices that land in parallel can wire it in unconditionally.
type Recorder struct {
	store Store
}

// NewRecorder wraps a Store. A nil store yields a no-op recorder.
func NewRecorder(store Store) *Recorder {
	if store == nil {
		return nil
	}
	return &Recorder{store: store}
}

// Store exposes the underlying store (nil for a no-op recorder).
func (r *Recorder) Store() Store {
	if r == nil {
		return nil
	}
	return r.store
}

// StartRun records determinism metadata for a lease's run. Call at grant.
func (r *Recorder) StartRun(l proto.Lease, target *proto.Target, daemonVersion string) {
	if r == nil {
		return
	}
	meta := RunMeta{
		FormatVersion: FormatVersion,
		RunID:         l.ID,
		LeaseID:       l.ID,
		AgentID:       l.AgentID,
		Purpose:       l.Purpose,
		DaemonVersion: daemonVersion,
		CreatedAt:     l.CreatedAt,
	}
	if target != nil {
		meta.TargetUDID = target.UDID
		meta.TargetName = target.Name
		meta.Runtime = target.Runtime
		meta.DeviceType = target.DeviceType
	}
	_ = r.store.WriteMeta(l.ID, meta)
}

// Event describes one recorded protocol action.
type Event struct {
	Kind    string // entry kind: "lease", "action", "observation", "state", "segment"
	LeaseID string // run ID
	AgentID string
	Action  string         // e.g. "leases.acquire", "targets.boot", "hid.tap"
	Params  map[string]any // request params (opaque)
	Status  string         // "ok" or "error"
	Error   string         // error message when Status == "error"
	// AXBefore / AXAfter are a11y tree hashes when the actions backend
	// provides them; empty otherwise.
	AXBefore  string
	AXAfter   string
	Artifacts []ArtifactRef
	Extra     map[string]any // merged into the payload verbatim
}

// Record appends one entry for ev and returns its ref (zero on no-op/error).
// Recording is best-effort: it never fails the recorded operation.
func (r *Recorder) Record(ctx context.Context, ev Event) proto.JournalRef {
	if r == nil || ev.LeaseID == "" {
		return proto.JournalRef{}
	}
	payload := map[string]any{
		"lease_id": ev.LeaseID,
		"action":   ev.Action,
		"status":   ev.Status,
	}
	if ev.AgentID != "" {
		payload["agent_id"] = ev.AgentID
	}
	if len(ev.Params) > 0 {
		payload["params"] = ev.Params
	}
	if ev.Error != "" {
		payload["error"] = ev.Error
	}
	if ev.AXBefore != "" {
		payload["ax_before"] = ev.AXBefore
	}
	if ev.AXAfter != "" {
		payload["ax_after"] = ev.AXAfter
	}
	if len(ev.Artifacts) > 0 {
		refs := make([]any, 0, len(ev.Artifacts))
		for _, a := range ev.Artifacts {
			refs = append(refs, map[string]any{"path": a.Path, "sha256": a.SHA256, "bytes": a.Bytes})
		}
		payload["artifacts"] = refs
	}
	for k, v := range ev.Extra {
		payload[k] = v
	}
	kind := ev.Kind
	if kind == "" {
		kind = "action"
	}
	ref, err := r.store.Append(ctx, ev.LeaseID, kind, payload)
	if err != nil {
		return proto.JournalRef{}
	}
	return ref
}
