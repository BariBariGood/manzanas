package state

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

var (
	// ErrImageNotFound means no golden image matches the given ID or name.
	ErrImageNotFound = errors.New("image not found")
	// ErrSlimUnavailable means a slim profile was requested but the host
	// has no simslim binary (or no slim runner is wired).
	ErrSlimUnavailable = errors.New("simslim is not available on this host")
	// ErrImageCorrupt means an image's on-disk archive no longer matches
	// the digest recorded at build time — bad on-disk state, not a daemon
	// bug, so it maps to a non-500 wire error.
	ErrImageCorrupt = errors.New("image archive corrupt")
)

// Images is the golden-image store interface the server binds to the
// /v0/images routes. ImageStore is the simctl-backed implementation.
type Images interface {
	// Build creates, optionally slims, archives, and deletes a fresh
	// builder simulator, returning the stored image's metadata.
	Build(ctx context.Context, req proto.ImageBuildRequest) (proto.ImageInfo, error)
	// List returns all golden images in the on-disk index.
	List(ctx context.Context) ([]proto.ImageInfo, error)
	// Stamp creates count fresh simulators from image id (or name), each
	// named "<prefix>-<n>". It returns the resolved image's metadata so
	// callers stamping by name learn the actual image ID.
	Stamp(ctx context.Context, id string, count int, prefix string) (proto.ImageInfo, []proto.StampedSim, error)
	// Delete removes an image's archive and index entry.
	Delete(ctx context.Context, id string) error
}

// SlimFunc slims a booted simulator with a named simslim profile. It may
// be nil when the host has no simslim binary, in which case builds with a
// slim profile fail with ErrSlimUnavailable.
type SlimFunc func(ctx context.Context, udid, profile string) error

// SlimVerifyFunc checks that a booted sim's disable overrides exactly
// match the named simslim profile (`simslim verify`), returning an error
// listing the drift otherwise. It may be nil when the installed simslim
// predates the verify command; callers then fall back to launchctl
// print-disabled parsing.
type SlimVerifyFunc func(ctx context.Context, udid, profile string) error

// ImageStore builds and stamps golden images: archived (tar.zst),
// optionally slimmed simulator data directories that can be stamped out
// into fresh, leasable simulators in seconds. See docs/images.md.
type ImageStore struct {
	run  Runner
	dir  string // on-disk store, e.g. ~/.manzanasd/images
	idx  *imageIndex
	slim SlimFunc
	// slimVerify, when non-nil, is the exact profile-match check used in
	// preference to launchctl print-disabled parsing (see SetSlimVerify).
	slimVerify SlimVerifyFunc
	// slimmed records which sims were stamped from a slim image and what
	// must stay disabled on them, so ReapplySlim can restore the disables
	// after a `simctl erase` (which wipes the per-UDID launchctl config).
	slimmed *slimRegistry
	// slimCheck pre-validates a slim profile name without running simslim,
	// so a bad name fails before a builder sim is created. Optional.
	slimCheck func(profile string) error
	now       func() time.Time

	// mu serializes mutating image ops (build/stamp/delete) so a delete
	// can never yank an archive out from under a running stamp.
	mu sync.Mutex

	// sweepDone is closed when the construction-time orphan-sim sweep
	// finishes (immediately when run is nil).
	sweepDone chan struct{}
}

// NewImageStore creates an ImageStore rooted at dir (created on demand).
// slim may be nil when simslim is unavailable.
func NewImageStore(run Runner, dir string, slim SlimFunc) *ImageStore {
	sweepStaleImageFiles(dir)
	s := &ImageStore{
		run:       run,
		dir:       dir,
		idx:       newImageIndex(filepath.Join(dir, "images.json")),
		slim:      slim,
		slimmed:   newSlimRegistry(filepath.Join(dir, "slimmed.json")),
		now:       func() time.Time { return time.Now().UTC() },
		sweepDone: make(chan struct{}),
	}
	// The device sweep shells out to simctl and can take a while on a
	// loaded host, so it must not block daemon startup. It holds mu, so
	// it can never race a build/stamp into deleting a live builder.
	go func() {
		defer close(s.sweepDone)
		if run == nil {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		exists := sweepOrphanImageSims(run)
		// Sims deleted out of band are never erased again, so drop their
		// slim-registry entries or the file grows without bound. Never
		// trust an empty listing: transient CoreSimulator state can parse
		// as zero devices, and pruning on it would wipe every recorded
		// disable set (sims would come back un-slimmed after their next
		// erase).
		if len(exists) > 0 {
			_ = s.slimmed.Prune(exists)
		}
	}()
	return s
}

// SetSlimCheck installs an optional pre-flight profile validator (see
// HostSlimProfileCheck) run at the top of Build.
func (s *ImageStore) SetSlimCheck(fn func(profile string) error) {
	s.slimCheck = fn
}

// SetSlimVerify installs an optional exact profile-match verifier (see
// HostSlimVerifyFunc). When set, build and stamp verification runs it
// against the image's profile in addition to the launchctl-based
// disable-set check; when nil, the launchctl path stands alone.
func (s *ImageStore) SetSlimVerify(fn SlimVerifyFunc) {
	s.slimVerify = fn
}

// profileResolves reports whether the profile name resolves on this host
// (per the slimCheck validator, when wired). Stamping an image built
// elsewhere must not depend on the profile file being deployed locally,
// so callers skip the exact-match verify when this is false.
func (s *ImageStore) profileResolves(profile string) bool {
	return s.slimCheck == nil || s.slimCheck(profile) == nil
}

func newImageID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "img_" + hex.EncodeToString(b)
}

// fileSHA256 returns the hex SHA-256 digest of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sweepStaleImageFiles is a best-effort reclaim of leftovers from a
// crashed build/stamp: staging dirs ("<id>.stamp-*") and half-written
// archives ("*.tar.zst.tmp") are only ever live while an operation is in
// flight, so at construction time anything matching is orphaned junk.
func sweepStaleImageFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if (e.IsDir() && strings.Contains(name, ".stamp-")) ||
			(!e.IsDir() && strings.HasSuffix(name, ".tar.zst.tmp")) {
			_ = os.RemoveAll(filepath.Join(dir, name))
		}
	}
}

// sweepOrphanImageSims is the device counterpart of sweepStaleImageFiles:
// hidden manzanasd-img-* sims exist only while a build/stamp is in flight
// in this process, so any found at construction time were stranded by a
// crash. Only shutdown devices are deleted; the flow is the sole creator
// of that name shape, so this never touches a sim it didn't make.
// Returns the set of UDIDs that remain on the host (nil when the device
// list could not be read).
func sweepOrphanImageSims(run Runner) map[string]bool {
	if run == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := run.Simctl(ctx, "list", "devices", "--json")
	if err != nil {
		return nil
	}
	var parsed struct {
		Devices map[string][]struct {
			UDID  string `json:"udid"`
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil
	}
	exists := map[string]bool{}
	for _, devs := range parsed.Devices {
		for _, d := range devs {
			if strings.HasPrefix(d.Name, proto.ImageDeviceNamePrefix) && d.State == "Shutdown" {
				_, _ = run.Simctl(ctx, "delete", d.UDID)
				continue
			}
			exists[d.UDID] = true
		}
	}
	return exists
}

// imageSpecRe bounds the wire-supplied device type and runtime to the
// characters CoreSimulator identifiers ("com.apple.CoreSimulator.…")
// and display names ("iPhone 17", "iOS 26.5") actually use, before they
// are handed to simctl.
var imageSpecRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._()-]{0,127}$`)

// cleanupCtx bounds detached cleanup simctl calls (cf. bootDetached):
// they must survive request cancellation but a hung simctl must become a
// dropped cleanup, not an indefinite hold on the image-store mutex.
func cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
}

// archivePath is the tar.zst archive for image id.
func (s *ImageStore) archivePath(id string) string {
	return filepath.Join(s.dir, id+".tar.zst")
}

// Build creates a fresh simulator of the given device type + runtime,
// optionally slims it with the named simslim profile, shuts it down,
// archives its data directory, and deletes the builder sim. The builder
// sim is created by (and only ever touched by) this flow.
func (s *ImageStore) Build(ctx context.Context, req proto.ImageBuildRequest) (proto.ImageInfo, error) {
	if req.DeviceType == "" || req.Runtime == "" {
		return proto.ImageInfo{}, fmt.Errorf("%w: device_type and runtime are required", ErrBadImageRequest)
	}
	if !imageSpecRe.MatchString(req.DeviceType) {
		return proto.ImageInfo{}, fmt.Errorf("%w: device_type must match %s", ErrBadImageRequest, imageSpecRe)
	}
	if !imageSpecRe.MatchString(req.Runtime) {
		return proto.ImageInfo{}, fmt.Errorf("%w: runtime must match %s", ErrBadImageRequest, imageSpecRe)
	}
	if req.SlimProfile != "" && s.slim == nil {
		return proto.ImageInfo{}, ErrSlimUnavailable
	}
	// simslim silently no-ops on runtimes before iOS 18 — refuse rather
	// than archive an unslimmed image labelled slim.
	if req.SlimProfile != "" {
		if err := slimRuntimeGuard(req.Runtime); err != nil {
			return proto.ImageInfo{}, err
		}
	}
	// Resolve the profile before paying for a builder sim: a typo'd name
	// is the client's fault and must fail fast as a bad request.
	if req.SlimProfile != "" && s.slimCheck != nil {
		if err := s.slimCheck(req.SlimProfile); err != nil {
			return proto.ImageInfo{}, fmt.Errorf("%w: %v", ErrBadImageRequest, err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// simctl create only accepts runtime identifiers; the registry (and
	// this API's metadata) use display names, so resolve here.
	runtimeID, err := resolveRuntime(ctx, s.run, req.Runtime)
	if err != nil {
		return proto.ImageInfo{}, err
	}

	id := newImageID()
	builderName := proto.ImageDeviceNamePrefix + id
	out, err := s.run.Simctl(ctx, "create", builderName, req.DeviceType, runtimeID)
	if err != nil {
		return proto.ImageInfo{}, simctlCreateErr(err)
	}
	udid := strings.TrimSpace(string(out))
	if err := validUDID(udid); err != nil {
		// The device exists even though its UDID didn't parse; delete it
		// by name so the hidden builder can't pile up on the host.
		dctx, cancel := cleanupCtx(ctx)
		defer cancel()
		_, _ = s.run.Simctl(dctx, "delete", builderName)
		return proto.ImageInfo{}, fmt.Errorf("simctl create returned %w", err)
	}
	// From here on the builder sim must never leak: delete it on any exit.
	defer func() {
		dctx, cancel := cleanupCtx(ctx)
		defer cancel()
		// Best-effort shutdown first: simctl cannot always delete a
		// device that is still booted (e.g. the slim step's shutdown
		// failed or was cancelled).
		_, _ = s.run.Simctl(dctx, "shutdown", udid)
		_, _ = s.run.Simctl(dctx, "delete", udid)
	}()

	var disabled []string
	postSlimProcs := 0
	if req.SlimProfile != "" {
		disabled, postSlimProcs, err = s.slimBuilder(ctx, udid, req.SlimProfile)
		if err != nil {
			return proto.ImageInfo{}, err
		}
	}
	// Guardrail: never archive a booted sim's data dir (torn state).
	if st, err := deviceState(ctx, s.run, udid); err != nil {
		return proto.ImageInfo{}, err
	} else if st != "Shutdown" {
		return proto.ImageInfo{}, fmt.Errorf("%w (builder state: %s)", ErrTargetBusy, st)
	}

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return proto.ImageInfo{}, err
	}
	dataDir := s.run.DeviceDataDir(udid)
	plist := filepath.Join(filepath.Dir(dataDir), "device.plist")
	size, err := packImage(ctx, s.archivePath(id), dataDir, plist)
	if err != nil {
		_ = os.Remove(s.archivePath(id))
		return proto.ImageInfo{}, err
	}
	sum, err := fileSHA256(s.archivePath(id))
	if err != nil {
		_ = os.Remove(s.archivePath(id))
		return proto.ImageInfo{}, err
	}
	// Re-verify at commit time: the pre-pack state check is check-then-act
	// against other host agents, so a builder booted mid-pack means the
	// archive may hold torn state and must be discarded.
	if st, err := deviceState(ctx, s.run, udid); err != nil {
		_ = os.Remove(s.archivePath(id))
		return proto.ImageInfo{}, err
	} else if st != "Shutdown" {
		_ = os.Remove(s.archivePath(id))
		return proto.ImageInfo{}, fmt.Errorf("%w (builder state after archiving: %s)", ErrTargetBusy, st)
	}
	info := proto.ImageInfo{
		ID:               id,
		Name:             req.Name,
		DeviceType:       req.DeviceType,
		Runtime:          req.Runtime,
		SlimProfile:      req.SlimProfile,
		DisabledServices: disabled,
		DisabledCount:    len(disabled),
		PostSlimProcs:    postSlimProcs,
		SizeBytes:        size,
		SHA256:           sum,
		CreatedAt:        s.now(),
	}
	if err := s.idx.Add(info); err != nil {
		_ = os.Remove(s.archivePath(id))
		return proto.ImageInfo{}, err
	}
	return info, nil
}

// slimBuilder boots the builder sim, runs simslim, captures the resulting
// launchctl disable set and running-service count while the builder is
// still booted, and shuts it down again (simslim requires a booted
// target). A slim that leaves zero services disabled failed — archiving
// it would produce a stock sim labelled slim.
func (s *ImageStore) slimBuilder(ctx context.Context, udid, profile string) (disabled []string, postSlimProcs int, err error) {
	if _, err := s.run.Simctl(ctx, "boot", udid); err != nil {
		return nil, 0, err
	}
	slimErr := s.slim(ctx, udid, profile)
	// Exact profile-match check (`simslim verify`) when the installed
	// simslim supports it; drift fails the build with the drifted daemons
	// listed. The launchctl capture below still runs either way — the
	// disable set is what stamp/re-apply restore after `simctl erase`.
	if slimErr == nil && s.slimVerify != nil {
		slimErr = s.slimVerify(ctx, udid, profile)
	}
	if slimErr == nil {
		var have map[string]bool
		have, slimErr = printDisabledServices(ctx, s.run, udid)
		if slimErr == nil {
			for svc := range have {
				disabled = append(disabled, svc)
			}
			sort.Strings(disabled)
			if len(disabled) == 0 {
				slimErr = errors.New("slim left zero services disabled — the runtime likely ignores simslim")
			} else {
				postSlimProcs = runningServiceCount(ctx, s.run, udid)
			}
		}
	}
	if _, err := s.run.Simctl(ctx, "shutdown", udid); err != nil &&
		!strings.Contains(err.Error(), "current state: Shutdown") {
		if slimErr == nil {
			return nil, 0, err
		}
	}
	if slimErr != nil {
		return nil, 0, fmt.Errorf("simslim (profile %q): %w", profile, slimErr)
	}
	return disabled, postSlimProcs, nil
}

// List returns all golden images in the index.
func (s *ImageStore) List(ctx context.Context) ([]proto.ImageInfo, error) {
	return s.idx.List()
}

// Delete removes an image's archive and index entry. Simulators already
// stamped from the image are untouched (they own full copies).
func (s *ImageStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := s.idx.Resolve(id)
	if err != nil {
		return err
	}
	// Drop the index entry first: a failure then strands a harmless
	// orphan archive, never a listed image whose archive is gone.
	if _, err := s.idx.Remove(info.ID); err != nil {
		return err
	}
	if err := os.Remove(s.archivePath(info.ID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ErrBadImageRequest means an image build/stamp request is malformed.
var ErrBadImageRequest = errors.New("bad image request")
