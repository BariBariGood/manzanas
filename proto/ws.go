package proto

import "encoding/json"

// Envelope is the framing for every JSON message on the WebSocket surface.
//
// Client -> server messages set Method and ID (for request/response
// correlation). Server -> client responses echo ID and set Result or Error.
// Server-initiated events set Event and carry Result.
type Envelope struct {
	V      string          `json:"v"` // protocol version, "v0"
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Event  string          `json:"event,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// WS method names (see PROTOCOL.md §4).
const (
	MethodListTargets  = "targets.list"
	MethodAcquireLease = "leases.acquire"
	MethodRenewLease   = "leases.renew"
	MethodReleaseLease = "leases.release"
	MethodGetLease     = "leases.get"
	MethodBootTarget   = "targets.boot"
	MethodShutdown     = "targets.shutdown"
	MethodAction       = "actions.dispatch"
	MethodActionBatch  = "actions.batch"
	MethodStreamOpen   = "streams.open"
	MethodJournalTail  = "journal.tail"

	// Recording methods (owned by the video-capture slice).
	MethodRecordingStart = "recording.start"
	MethodRecordingStop  = "recording.stop"

	// State engine methods (owned by the state slice).
	MethodStateSnapshot        = "state.snapshot"
	MethodStateRestore         = "state.restore"
	MethodStateErase           = "state.erase"
	MethodStateFixture         = "state.fixture"
	MethodStateSnapshotsList   = "state.snapshots.list"
	MethodStateSnapshotsDelete = "state.snapshots.delete"

	// Golden-image methods (owned by the state slice).
	MethodImagesBuild  = "images.build"
	MethodImagesList   = "images.list"
	MethodImagesStamp  = "images.stamp"
	MethodImagesDelete = "images.delete"
)

// Server-initiated event names.
const (
	EventLeaseGranted = "lease.granted" // a queued lease became active
	EventLeaseExpired = "lease.expired"
	EventTargetState  = "target.state"  // a target changed state
	EventJournalEntry = "journal.entry" // one journal entry, on a journal.tail subscription
)
