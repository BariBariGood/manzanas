// Package state implements the deterministic environment-control engine:
// snapshot/restore of shutdown simulators, fixtures (status bar, privacy,
// push, locale, timezone, URL), and per-lease auto-reset.
//
// See docs/state.md for the snapshot mechanism and its tradeoffs.
package state

import (
	"context"
	"errors"

	"github.com/BariBariGood/manzanas/proto"
)

var (
	// ErrTargetBusy means the target is booted and the requested operation
	// requires it to be shutdown (pass reboot=true to shutdown+restore+boot).
	ErrTargetBusy = errors.New("target is booted; operation requires shutdown")
	// ErrSnapshotNotFound means no snapshot matches the given ID or label.
	ErrSnapshotNotFound = errors.New("snapshot not found")
	// ErrBadFixture means the fixture name is unknown or the payload is invalid.
	ErrBadFixture = errors.New("unknown fixture or invalid payload")
	// ErrBadReset means a reset spec is not one of none|erase|snapshot:<name>.
	ErrBadReset = errors.New("reset must be none, erase, or snapshot:<name>")
)

// Engine snapshots/restores simulator state and applies fixtures
// (permissions, status bar, locale, time, push). Fixture payloads are
// opaque maps owned by the state slice.
//
// Extended from the foundation stub: Snapshot takes a label and returns
// full metadata; Restore reports whether a reboot cycle was performed;
// ListSnapshots/DeleteSnapshot manage the on-disk index; Erase and Reset
// support per-lease auto-reset.
type Engine interface {
	// Snapshot captures a shutdown target's state and returns its metadata.
	Snapshot(ctx context.Context, udid, label string) (proto.SnapshotInfo, error)
	// Restore restores a target to a previously captured snapshot. The
	// target must be shutdown; if it is booted and reboot is true, the
	// engine shuts it down, restores, and boots it again (returning true).
	// If it is booted and reboot is false, Restore fails with ErrTargetBusy.
	Restore(ctx context.Context, udid, snapshotID string, reboot bool) (rebooted bool, err error)
	// ListSnapshots returns all snapshots in the on-disk index.
	ListSnapshots(ctx context.Context) ([]proto.SnapshotInfo, error)
	// DeleteSnapshot removes a snapshot (clone device + index entry).
	DeleteSnapshot(ctx context.Context, snapshotID string) error
	// Erase factory-resets a shutdown target (simctl erase).
	Erase(ctx context.Context, udid string) error
	// Reset applies a lease reset spec ("erase" or "snapshot:<name>") to a
	// target, shutting it down first if needed and leaving it shutdown.
	// Used by the lease manager's release/expiry hook.
	Reset(ctx context.Context, udid, spec string) error
	// ApplyFixture applies a named fixture (e.g. "statusbar", "privacy",
	// "push", "locale", "timezone", "url") with an opaque payload.
	ApplyFixture(ctx context.Context, udid, name string, payload map[string]any) error
}

// Reset spec constants (proto.AcquireLeaseRequest.Reset).
const (
	ResetNone           = "none"
	ResetErase          = "erase"
	ResetSnapshotPrefix = "snapshot:"
)

// ValidResetSpec reports whether spec is one of ""|none|erase|snapshot:<name>.
func ValidResetSpec(spec string) bool {
	switch {
	case spec == "" || spec == ResetNone || spec == ResetErase:
		return true
	case len(spec) > len(ResetSnapshotPrefix) && spec[:len(ResetSnapshotPrefix)] == ResetSnapshotPrefix:
		return true
	}
	return false
}
