package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// SimEngine implements Engine for iOS simulators via simctl. Snapshots are
// simctl clones of shutdown sims, tracked in a JSON index; restore swaps
// the target's data directory with an APFS clonefile copy of the clone's.
// See docs/state.md.
type SimEngine struct {
	run      Runner
	idx      *snapshotIndex
	fixtures map[string]Fixture
	now      func() time.Time
	// postErase, when set, runs after every successful erase (Erase and
	// Reset's erase spec). Wired to ImageStore.ReapplySlim: `simctl erase`
	// wipes the per-UDID launchctl disable config, so a sim stamped from a
	// slim image must have its disables re-applied or it boots stock.
	postErase func(ctx context.Context, udid string) error

	// locksMu guards locks, which serializes destructive ops per target so
	// e.g. a client restore and an auto-reset overlapping on one device
	// can't clobber each other's staging/backup directories in ReplaceDir.
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

// NewSimEngine creates a SimEngine. indexPath is the JSON snapshot index
// (e.g. ~/.manzanasd/snapshots.json).
func NewSimEngine(run Runner, indexPath string) *SimEngine {
	return &SimEngine{
		run:      run,
		idx:      newSnapshotIndex(indexPath),
		fixtures: builtinFixtures(),
		now:      func() time.Time { return time.Now().UTC() },
		locks:    make(map[string]*sync.Mutex),
	}
}

// SetPostErase installs a hook run after every successful erase, with
// the per-target lock held (see postErase).
func (e *SimEngine) SetPostErase(fn func(ctx context.Context, udid string) error) {
	e.postErase = fn
}

// lockTarget acquires the per-target mutex, creating it on first use, and
// returns the unlock func.
func (e *SimEngine) lockTarget(udid string) func() {
	e.locksMu.Lock()
	mu, ok := e.locks[udid]
	if !ok {
		mu = &sync.Mutex{}
		e.locks[udid] = mu
	}
	e.locksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// udidPattern matches CoreSimulator device UDIDs (uppercase hex UUIDs).
// Everything used to build a device data-dir path must match it, so a
// corrupted or tampered snapshot index can never yield a path outside the
// devices tree (e.g. "../../..").
var udidPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}(-[0-9A-Fa-f]{4}){3}-[0-9A-Fa-f]{12}$`)

func validUDID(udid string) error {
	if !udidPattern.MatchString(udid) {
		return fmt.Errorf("invalid device UDID %q", udid)
	}
	return nil
}

func newSnapshotID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "snp_" + hex.EncodeToString(b)
}

// deviceState returns the simctl state ("Booted", "Shutdown", ...) of udid.
func (e *SimEngine) deviceState(ctx context.Context, udid string) (string, error) {
	return deviceState(ctx, e.run, udid)
}

// deviceState returns the simctl state of udid via run (shared by the
// engine and the image store).
func deviceState(ctx context.Context, run Runner, udid string) (string, error) {
	out, err := run.Simctl(ctx, "list", "devices", "--json")
	if err != nil {
		return "", err
	}
	var parsed struct {
		Devices map[string][]struct {
			UDID  string `json:"udid"`
			State string `json:"state"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return "", fmt.Errorf("parse simctl output: %w", err)
	}
	for _, devices := range parsed.Devices {
		for _, d := range devices {
			if d.UDID == udid {
				return d.State, nil
			}
		}
	}
	return "", fmt.Errorf("target %s: %w", udid, errDeviceNotFound)
}

var errDeviceNotFound = errors.New("device not found")

// requireClone maps a snapshot whose backing clone device vanished out of
// band (Xcode cleanup, `simctl delete unavailable`) to ErrSnapshotNotFound,
// so restores answer not_found and auto-resets degrade to erase instead of
// quarantining the target.
func (e *SimEngine) requireClone(ctx context.Context, info proto.SnapshotInfo) error {
	_, err := e.deviceState(ctx, info.CloneUDID)
	if errors.Is(err, errDeviceNotFound) {
		return fmt.Errorf("%w: %s's backing clone device is gone", ErrSnapshotNotFound, info.ID)
	}
	return err
}

func (e *SimEngine) requireShutdown(ctx context.Context, udid string) error {
	st, err := e.deviceState(ctx, udid)
	if err != nil {
		return err
	}
	if st != "Shutdown" {
		return fmt.Errorf("%w (state: %s)", ErrTargetBusy, st)
	}
	return nil
}

// Snapshot clones a shutdown sim into a hidden snapshot device and records
// it in the index.
func (e *SimEngine) Snapshot(ctx context.Context, udid, label string) (proto.SnapshotInfo, error) {
	defer e.lockTarget(udid)()
	if err := e.requireShutdown(ctx, udid); err != nil {
		return proto.SnapshotInfo{}, err
	}
	id := newSnapshotID()
	cloneName := proto.SnapshotDeviceNamePrefix + id
	out, err := e.run.Simctl(ctx, "clone", udid, cloneName)
	if err != nil {
		return proto.SnapshotInfo{}, err
	}
	cloneUDID := strings.TrimSpace(string(out))
	if err := validUDID(cloneUDID); err != nil {
		_, _ = e.run.Simctl(ctx, "delete", cloneName)
		return proto.SnapshotInfo{}, fmt.Errorf("simctl clone returned %w", err)
	}
	info := proto.SnapshotInfo{
		ID:         id,
		SourceUDID: udid,
		CloneUDID:  cloneUDID,
		Label:      label,
		CreatedAt:  e.now(),
	}
	if err := e.idx.Add(info); err != nil {
		// Index write failed: don't leak the clone device.
		_, _ = e.run.Simctl(ctx, "delete", cloneUDID)
		return proto.SnapshotInfo{}, err
	}
	return info, nil
}

// Restore replaces the target's data directory with a copy of the snapshot
// clone's. The target must be shutdown, or booted with reboot=true.
func (e *SimEngine) Restore(ctx context.Context, udid, snapshotID string, reboot bool) (bool, error) {
	defer e.lockTarget(udid)()
	info, err := e.idx.Resolve(udid, snapshotID)
	if err != nil {
		return false, err
	}
	if info.SourceUDID != udid {
		return false, fmt.Errorf("snapshot %s was taken from target %s, not %s", info.ID, info.SourceUDID, udid)
	}
	if err := e.requireClone(ctx, info); err != nil {
		return false, err
	}
	// Validate paths before shutting anything down, so a bad index entry
	// can't leave a running target powered off.
	src, dst, err := e.dataDirs(info.CloneUDID, udid)
	if err != nil {
		return false, err
	}
	st, err := e.deviceState(ctx, udid)
	if err != nil {
		return false, err
	}
	rebooted := false
	if st != "Shutdown" {
		if !reboot {
			return false, fmt.Errorf("%w (state: %s)", ErrTargetBusy, st)
		}
		if _, err := e.run.Simctl(ctx, "shutdown", udid); err != nil && !strings.Contains(err.Error(), "current state: Shutdown") {
			return false, err
		}
		rebooted = true
	}
	if err := e.run.ReplaceDir(ctx, src, dst); err != nil {
		if rebooted {
			// Best effort: the caller opted into the reboot cycle, so don't
			// leave a previously booted target powered off (ReplaceDir rolls
			// back the data dir on failure).
			_ = e.bootDetached(ctx, udid)
		}
		return rebooted, err
	}
	if rebooted {
		if err := e.bootDetached(ctx, udid); err != nil {
			return true, err
		}
	}
	return rebooted, nil
}

// bootDetached boots udid on a bounded context detached from ctx, so the
// reboot half of a restore still runs when the multi-GB copy outlived the
// request (client disconnect/timeout cancels ctx).
func (e *SimEngine) bootDetached(ctx context.Context, udid string) error {
	bootCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	_, err := e.run.Simctl(bootCtx, "boot", udid)
	return err
}

// ListSnapshots returns the index contents.
func (e *SimEngine) ListSnapshots(ctx context.Context) ([]proto.SnapshotInfo, error) {
	return e.idx.List()
}

// DeleteSnapshot removes the clone device, then the index entry, so a
// failed simctl delete never orphans an untracked clone.
func (e *SimEngine) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	info, err := e.idx.Resolve("", snapshotID)
	if err != nil {
		return err
	}
	if _, err := e.run.Simctl(ctx, "delete", info.CloneUDID); err != nil && !deviceGone(err) {
		return err
	}
	_, err = e.idx.Remove(info.ID)
	return err
}

// deviceGone reports whether a simctl error means the device no longer
// exists (deleted out of band, `simctl delete unavailable`, Xcode
// cleanup). Deleting such a snapshot must still reclaim its index entry.
func deviceGone(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "invalid device") || strings.Contains(msg, "not found")
}

// Erase factory-resets a shutdown target.
func (e *SimEngine) Erase(ctx context.Context, udid string) error {
	defer e.lockTarget(udid)()
	if err := e.requireShutdown(ctx, udid); err != nil {
		return err
	}
	if _, err := e.run.Simctl(ctx, "erase", udid); err != nil {
		return err
	}
	return e.runPostErase(ctx, udid)
}

// runPostErase invokes the postErase hook, if any. The erase itself is
// destructive (it wipes the per-UDID launchctl disable config) and the
// hook is the only thing restoring it, so — like bootDetached — it runs
// on a bounded context detached from ctx: a client disconnect must not
// abort the recovery half once the erase has happened. The budget tracks
// the caller's remaining deadline so a reset normally stays within the
// documented 10-minute bound (see resetTimeout / PROTOCOL.md §7), but
// never drops below a floor that fits one boot/verify/shutdown cycle —
// a slow erase must not starve the only step that restores the disable
// set (an expired budget would fail the reset and quarantine the target
// while leaving it un-slimmed).
const postEraseFloor = 3 * time.Minute

func (e *SimEngine) runPostErase(ctx context.Context, udid string) error {
	if e.postErase == nil {
		return nil
	}
	budget := 10 * time.Minute
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem < budget {
			budget = rem
		}
	}
	if budget < postEraseFloor {
		budget = postEraseFloor
	}
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	if err := e.postErase(hctx, udid); err != nil {
		return fmt.Errorf("post-erase re-slim: %w", err)
	}
	return nil
}

// Reset applies a lease reset spec, shutting the target down first if
// needed and leaving it shutdown for the next holder.
func (e *SimEngine) Reset(ctx context.Context, udid, spec string) error {
	defer e.lockTarget(udid)()
	switch {
	case spec == "" || spec == ResetNone:
		return nil
	case spec == ResetErase:
		if _, err := e.run.Simctl(ctx, "shutdown", udid); err != nil && !strings.Contains(err.Error(), "current state: Shutdown") {
			return err
		}
		if _, err := e.run.Simctl(ctx, "erase", udid); err != nil {
			return err
		}
		return e.runPostErase(ctx, udid)
	case strings.HasPrefix(spec, ResetSnapshotPrefix):
		name := strings.TrimPrefix(spec, ResetSnapshotPrefix)
		if _, err := e.run.Simctl(ctx, "shutdown", udid); err != nil && !strings.Contains(err.Error(), "current state: Shutdown") {
			return err
		}
		info, err := e.idx.Resolve(udid, name)
		if err != nil {
			return err
		}
		if err := e.requireClone(ctx, info); err != nil {
			return err
		}
		src, dst, err := e.dataDirs(info.CloneUDID, udid)
		if err != nil {
			return err
		}
		return e.run.ReplaceDir(ctx, src, dst)
	default:
		return ErrBadReset
	}
}

// dataDirs validates both UDIDs before deriving filesystem paths from
// them, guarding ReplaceDir against traversal via a tampered index.
func (e *SimEngine) dataDirs(cloneUDID, udid string) (src, dst string, err error) {
	if err := validUDID(cloneUDID); err != nil {
		return "", "", fmt.Errorf("snapshot index: %w", err)
	}
	if err := validUDID(udid); err != nil {
		return "", "", err
	}
	return e.run.DeviceDataDir(cloneUDID), e.run.DeviceDataDir(udid), nil
}

// ApplyFixture dispatches to the named fixture implementation.
func (e *SimEngine) ApplyFixture(ctx context.Context, udid, name string, payload map[string]any) error {
	f, ok := e.fixtures[name]
	if !ok {
		return fmt.Errorf("%w: %q (known: %s)", ErrBadFixture, name, strings.Join(fixtureNames(e.fixtures), ", "))
	}
	return f.Apply(ctx, e.run, udid, payload)
}
