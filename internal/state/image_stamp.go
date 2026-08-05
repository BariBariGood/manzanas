package state

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BariBariGood/manzanas/proto"
)

// maxStampCount bounds one stamp request; stamping is cheap but each sim
// still costs disk, and a typo'd count must not fill the host.
const maxStampCount = 16

// stampPrefixRe bounds stamped device names to a sane charset: the prefix
// comes off the wire and ends up as a simctl device name.
var stampPrefixRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// deviceNames returns the set of device names currently known to simctl.
func deviceNames(ctx context.Context, run Runner) (map[string]bool, error) {
	out, err := run.Simctl(ctx, "list", "devices", "--json")
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Devices map[string][]struct {
			Name string `json:"name"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parse simctl list: %w", err)
	}
	names := make(map[string]bool)
	for _, devs := range parsed.Devices {
		for _, d := range devs {
			names[d.Name] = true
		}
	}
	return names, nil
}

// deviceStateMap returns udid -> state for every device simctl knows, so
// a batch of sims can be re-verified with one list call.
func deviceStateMap(ctx context.Context, run Runner) (map[string]string, error) {
	out, err := run.Simctl(ctx, "list", "devices", "--json")
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Devices map[string][]struct {
			UDID  string `json:"udid"`
			State string `json:"state"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parse simctl list: %w", err)
	}
	states := make(map[string]string)
	for _, devs := range parsed.Devices {
		for _, d := range devs {
			states[d.UDID] = d.State
		}
	}
	return states, nil
}

// Stamp creates count fresh simulators from a golden image by creating a
// sim of the image's device type/runtime and replacing its data directory
// with the archived one (the same Runner.ReplaceDir plumbing the state
// engine's restore uses). Each new sim is named "<prefix>-<n>" and shows
// up in the registry as a leasable target. Only sims created by this call
// are ever touched; on a partial failure, the sims already created are
// deleted again.
func (s *ImageStore) Stamp(ctx context.Context, id string, count int, prefix string) (proto.ImageInfo, []proto.StampedSim, error) {
	if count < 1 || count > maxStampCount {
		return proto.ImageInfo{}, nil, fmt.Errorf("%w: count must be 1..%d", ErrBadImageRequest, maxStampCount)
	}
	if prefix == "" {
		prefix = "manzanas"
	}
	if !stampPrefixRe.MatchString(prefix) {
		return proto.ImageInfo{}, nil, fmt.Errorf("%w: name_prefix must match %s", ErrBadImageRequest, stampPrefixRe)
	}
	// Guard the generated device name, not just the raw prefix:
	// "manzanasd-img" would otherwise yield hidden "manzanasd-img-<n>" sims.
	if strings.HasPrefix(prefix+"-", proto.SnapshotDeviceNamePrefix) || strings.HasPrefix(prefix+"-", proto.ImageDeviceNamePrefix) {
		return proto.ImageInfo{}, nil, fmt.Errorf("%w: name_prefix collides with a reserved device-name prefix", ErrBadImageRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := s.idx.Resolve(id)
	if err != nil {
		return proto.ImageInfo{}, nil, err
	}

	// A slim image without a recorded disable set (built before disables
	// were captured) cannot be verified: stamping it would silently hand
	// out unslimmed sims labelled slim. Rebuild the image instead.
	if info.SlimProfile != "" && len(info.DisabledServices) == 0 {
		return proto.ImageInfo{}, nil, fmt.Errorf("%w: image %s has slim_profile %q but no recorded disabled_services; rebuild the image to capture its disable set", ErrImageCorrupt, info.ID, info.SlimProfile)
	}

	// Refuse to provision from an archive that no longer matches the
	// digest recorded at build time: archives are plain files, and a
	// swapped or corrupted one must not become leasable sim state.
	// (Images built before digests were recorded have no SHA256.)
	if info.SHA256 != "" {
		sum, err := fileSHA256(s.archivePath(info.ID))
		if err != nil {
			return proto.ImageInfo{}, nil, err
		}
		if sum != info.SHA256 {
			return proto.ImageInfo{}, nil, fmt.Errorf("%w: image %s: archive digest mismatch (index %s, file %s)", ErrImageCorrupt, info.ID, info.SHA256, sum)
		}
	}

	// Extract the archive once into a staging dir; every stamped sim is a
	// ReplaceDir copy of it.
	staging, err := os.MkdirTemp(s.dir, info.ID+".stamp-")
	if err != nil {
		return proto.ImageInfo{}, nil, err
	}
	defer os.RemoveAll(staging)
	stagedData := filepath.Join(staging, "data")
	if err := unpackImage(ctx, s.archivePath(info.ID), staging); err != nil {
		return proto.ImageInfo{}, nil, err
	}

	// simctl create only accepts runtime identifiers; images built from a
	// display name store the display name, so resolve it per stamp.
	runtimeID, err := resolveRuntime(ctx, s.run, info.Runtime)
	if err != nil {
		return proto.ImageInfo{}, nil, err
	}

	// Number the public names past any existing <prefix>-<n> device, so
	// repeated stamps with the same prefix (including the default) never
	// produce duplicate device names.
	taken, err := deviceNames(ctx, s.run)
	if err != nil {
		return proto.ImageInfo{}, nil, err
	}
	names := make([]string, 0, count)
	for n := 1; len(names) < count; n++ {
		name := fmt.Sprintf("%s-%d", prefix, n)
		if !taken[name] {
			names = append(names, name)
		}
	}

	// Phase 1: create + provision every sim under a hidden manzanasd-img-*
	// name, so the registry never enumerates (and the lease manager never
	// hands out) a half-provisioned sim, and cleanup only ever deletes
	// sims that were never visible.
	var created []proto.StampedSim
	cleanup := func() {
		cctx, cancel := cleanupCtx(ctx)
		defer cancel()
		udids := make([]string, 0, len(created))
		for _, c := range created {
			_, _ = s.run.Simctl(cctx, "shutdown", c.UDID)
			_, _ = s.run.Simctl(cctx, "delete", c.UDID)
			udids = append(udids, c.UDID)
		}
		// Deleted sims are never erased again, so drop any registry
		// entries the record step below already wrote for them.
		_ = s.slimmed.ForgetBatch(udids)
	}
	for n := 1; n <= count; n++ {
		hidden := fmt.Sprintf("%sstamp-%s-%d", proto.ImageDeviceNamePrefix, info.ID, n)
		publicName := names[n-1]
		out, err := s.run.Simctl(ctx, "create", hidden, info.DeviceType, runtimeID)
		if err != nil {
			cleanup()
			return proto.ImageInfo{}, nil, simctlCreateErr(err)
		}
		udid := strings.TrimSpace(string(out))
		if err := validUDID(udid); err != nil {
			// The device exists even though its UDID didn't parse; delete
			// it by name before cleaning up the rest.
			dctx, cancel := cleanupCtx(ctx)
			_, _ = s.run.Simctl(dctx, "delete", hidden)
			cancel()
			cleanup()
			return proto.ImageInfo{}, nil, fmt.Errorf("simctl create returned %w", err)
		}
		created = append(created, proto.StampedSim{UDID: udid, Name: publicName})
		// Guardrail: a freshly created sim is shutdown; refuse to swap the
		// data dir of anything booted.
		if st, err := deviceState(ctx, s.run, udid); err != nil {
			cleanup()
			return proto.ImageInfo{}, nil, err
		} else if st != "Shutdown" {
			cleanup()
			return proto.ImageInfo{}, nil, fmt.Errorf("%w (stamped sim %s state: %s)", ErrTargetBusy, udid, st)
		}
		if err := s.run.ReplaceDir(ctx, stagedData, s.run.DeviceDataDir(udid)); err != nil {
			cleanup()
			return proto.ImageInfo{}, nil, err
		}
		// launchctl disables are keyed to the sim UDID outside the data
		// dir (iOS 18+), so the swap above did NOT carry the image's slim
		// state: a stamped sim would boot effectively stock (all daemons
		// back). Re-apply the disable set captured at build time and
		// verify it took, while the sim is still hidden; a sim that can't
		// be slimmed fails the whole stamp rather than leasing out an
		// unslimmed sim labelled slim.
		if info.SlimProfile != "" {
			if err := s.reslimStamped(ctx, udid, info.SlimProfile, info.DisabledServices); err != nil {
				cleanup()
				return proto.ImageInfo{}, nil, fmt.Errorf("stamped sim %s: %w", udid, err)
			}
		}
	}

	// Re-verify at commit time: the per-sim check above is check-then-act
	// against other host agents (anything can `simctl boot` a hidden sim
	// between the check and the swap), so before any sim becomes visible,
	// confirm every one is still Shutdown — a booted one means its data
	// dir may have been swapped underneath a running sim (torn state), and
	// the whole stamp rolls back with target_busy.
	states, err := deviceStateMap(ctx, s.run)
	if err != nil {
		cleanup()
		return proto.ImageInfo{}, nil, err
	}
	for _, c := range created {
		if st := states[c.UDID]; st != "Shutdown" {
			cleanup()
			return proto.ImageInfo{}, nil, fmt.Errorf("%w (stamped sim %s state after provisioning: %s)", ErrTargetBusy, c.UDID, st)
		}
	}

	// Record the slim state of every sim (one batch write) BEFORE any
	// becomes visible: the registry is the only thing keeping a stamped
	// sim slim across `simctl erase`, so a write failure fails the stamp
	// rather than committing sims that would silently boot stock after
	// their first reset. cleanup() forgets the batch again on rollback.
	if info.SlimProfile != "" {
		udids := make([]string, len(created))
		for i, c := range created {
			udids[i] = c.UDID
		}
		if err := s.slimmed.RecordBatch(udids, info.DisabledServices); err != nil {
			cleanup()
			return proto.ImageInfo{}, nil, fmt.Errorf("record slim state: %w", err)
		}
	}

	// Phase 2: everything is fully provisioned — commit by renaming to the
	// user-facing names, at which point the sims become visible/leasable.
	// A rename failure rolls the whole stamp back (all-or-nothing): the
	// already-visible sims are first renamed back to their hidden names
	// (pulling them out of the registry so nothing new can lease them)
	// and then everything is deleted. A client that grabbed a lease in
	// the brief visible window sees its target vanish, exactly as if the
	// sim had been deleted by an operator.
	for i, c := range created {
		if _, err := s.run.Simctl(ctx, "rename", c.UDID, c.Name); err != nil {
			bg, cancel := cleanupCtx(ctx)
			for j := 0; j < i; j++ {
				h := fmt.Sprintf("%sstamp-%s-%d", proto.ImageDeviceNamePrefix, info.ID, j+1)
				_, _ = s.run.Simctl(bg, "rename", created[j].UDID, h)
			}
			cancel()
			cleanup()
			return proto.ImageInfo{}, nil, fmt.Errorf("rename stamped sim %s: %w", c.UDID, err)
		}
	}
	return info, created, nil
}

// reslimStamped boots a freshly stamped (still hidden) sim, ensures the
// image's disable set is applied (re-applying and re-verifying via
// ensureDisabled), and shuts it down again. Never SIGKILLs daemons —
// launchd respawn churn costs more than the daemon itself. When a
// SlimVerifyFunc is wired and the image's profile resolves on this host,
// the re-applied state is additionally checked for exact drift against
// the profile (`simslim verify`). The profile file is not a hard
// dependency of stamping: archives are copyable between hosts and the
// image's recorded disable set (already verified by ensureDisabled) is
// the source of truth, so an unresolvable profile skips the exact-match
// check instead of failing the stamp. On older simslim binaries the
// launchctl-based check stands alone.
func (s *ImageStore) reslimStamped(ctx context.Context, udid, profile string, services []string) error {
	if _, err := s.run.Simctl(ctx, "boot", udid); err != nil {
		return err
	}
	slimErr := ensureDisabled(ctx, s.run, udid, services)
	if slimErr == nil && s.slimVerify != nil && s.profileResolves(profile) {
		slimErr = s.slimVerify(ctx, udid, profile)
	}
	if _, err := s.run.Simctl(ctx, "shutdown", udid); err != nil &&
		!strings.Contains(err.Error(), "current state: Shutdown") {
		if slimErr == nil {
			return err
		}
	}
	return slimErr
}
