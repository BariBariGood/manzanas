package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

func newTestStore(t *testing.T, slim SlimFunc) (*ImageStore, *fsFakeRunner) {
	t.Helper()
	base := t.TempDir()
	run := newFSFakeRunner(filepath.Join(base, "devices"))
	if err := os.MkdirAll(run.base, 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewImageStore(run, filepath.Join(base, "images"), slim)
	<-s.sweepDone
	run.calls = nil // drop the construction-time sweep's list call
	return s, run
}

func buildReq() proto.ImageBuildRequest {
	return proto.ImageBuildRequest{DeviceType: "iPhone 17", Runtime: "iOS 26.5", Name: "clean"}
}

func TestImageBuild(t *testing.T) {
	s, run := newTestStore(t, nil)
	info, err := s.Build(context.Background(), buildReq())
	if err != nil {
		t.Fatal(err)
	}
	if info.ID == "" || info.DeviceType != "iPhone 17" || info.Runtime != "iOS 26.5" || info.Name != "clean" {
		t.Fatalf("bad info: %+v", info)
	}
	if info.SizeBytes <= 0 {
		t.Fatalf("expected nonzero archive size, got %d", info.SizeBytes)
	}
	if _, err := os.Stat(s.archivePath(info.ID)); err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	// Builder sim must not leak.
	if left := run.udidsWithPrefix(proto.ImageDeviceNamePrefix); len(left) != 0 {
		t.Fatalf("builder sim leaked: %v", left)
	}
	imgs, err := s.List(context.Background())
	if err != nil || len(imgs) != 1 || imgs[0].ID != info.ID {
		t.Fatalf("list: %v %+v", err, imgs)
	}
}

func TestImageBuildValidation(t *testing.T) {
	s, _ := newTestStore(t, nil)
	if _, err := s.Build(context.Background(), proto.ImageBuildRequest{Runtime: "iOS 26.5"}); !errors.Is(err, ErrBadImageRequest) {
		t.Fatalf("want ErrBadImageRequest, got %v", err)
	}
	req := buildReq()
	req.SlimProfile = "agent-qa"
	if _, err := s.Build(context.Background(), req); !errors.Is(err, ErrSlimUnavailable) {
		t.Fatalf("want ErrSlimUnavailable, got %v", err)
	}
}

func TestImageBuildSlim(t *testing.T) {
	var slimmed []string
	var run *fsFakeRunner
	slim := func(ctx context.Context, udid, profile string) error {
		slimmed = append(slimmed, udid+" "+profile)
		run.disableAll(udid)
		return nil
	}
	var s *ImageStore
	s, run = newTestStore(t, slim)
	req := buildReq()
	req.SlimProfile = "agent-qa"
	info, err := s.Build(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if info.SlimProfile != "agent-qa" || len(slimmed) != 1 {
		t.Fatalf("slim not applied: %+v %v", info, slimmed)
	}
	// Slim runs against a booted sim; the builder must be shut down again
	// before archiving and then deleted.
	var sawBoot, sawShutdown bool
	run.mu.Lock()
	for _, c := range run.calls {
		if c[0] == "boot" {
			sawBoot = true
		}
		if c[0] == "shutdown" {
			sawShutdown = true
		}
	}
	run.mu.Unlock()
	if !sawBoot || !sawShutdown {
		t.Fatalf("expected boot+shutdown around slim; calls=%v", run.calls)
	}
}

func TestImageBuildSlimFailureCleansUp(t *testing.T) {
	slim := func(ctx context.Context, udid, profile string) error {
		return errors.New("simslim exploded")
	}
	s, run := newTestStore(t, slim)
	req := buildReq()
	req.SlimProfile = "agent-qa"
	if _, err := s.Build(context.Background(), req); err == nil {
		t.Fatal("expected error")
	}
	if left := run.udidsWithPrefix(proto.ImageDeviceNamePrefix); len(left) != 0 {
		t.Fatalf("builder sim leaked after failure: %v", left)
	}
	if imgs, _ := s.List(context.Background()); len(imgs) != 0 {
		t.Fatalf("index should be empty, got %+v", imgs)
	}
}

func TestImageStamp(t *testing.T) {
	s, run := newTestStore(t, nil)
	ctx := context.Background()

	info, err := s.Build(ctx, buildReq())
	if err != nil {
		t.Fatal(err)
	}

	_, created, err := s.Stamp(ctx, info.ID, 2, "qa")
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || created[0].Name != "qa-1" || created[1].Name != "qa-2" {
		t.Fatalf("bad created: %+v", created)
	}
	for _, c := range created {
		b, err := os.ReadFile(filepath.Join(run.DeviceDataDir(c.UDID), "fresh.txt"))
		if err != nil || string(b) != "factory" {
			t.Fatalf("stamped sim %s data not from image: %v %q", c.UDID, err, b)
		}
		// device.plist must be the stamped sim's own, not the builder's.
		p, err := os.ReadFile(filepath.Join(run.base, c.UDID, "device.plist"))
		if err != nil || string(p) != "<plist>"+c.UDID+"</plist>" {
			t.Fatalf("stamped sim %s lost its identity: %v %q", c.UDID, err, p)
		}
		run.mu.Lock()
		st := run.states[c.UDID]
		run.mu.Unlock()
		if st != "Shutdown" {
			t.Fatalf("stamped sim %s should be shutdown, got %s", c.UDID, st)
		}
	}
	// Staging dirs must be cleaned up.
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("staging dir leaked: %s", e.Name())
		}
	}
}

func TestNewImageStoreSweepsStaleFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "img_abc.stamp-123")
	if err := os.MkdirAll(filepath.Join(stale, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "img_abc.tar.zst.tmp")
	if err := os.WriteFile(tmp, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "img_abc.tar.zst")
	if err := os.WriteFile(keep, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	NewImageStore(nil, dir, nil)
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale staging dir not swept: %v", err)
	}
	if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
		t.Fatalf("stale tmp archive not swept: %v", err)
	}
	if _, err := os.Lstat(keep); err != nil {
		t.Fatalf("real archive must survive the sweep: %v", err)
	}
}

func TestNewImageStoreSweepsOrphanSims(t *testing.T) {
	base := t.TempDir()
	run := newFSFakeRunner(filepath.Join(base, "devices"))
	if err := os.MkdirAll(run.base, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out, err := run.Simctl(ctx, "create", proto.ImageDeviceNamePrefix+"img_dead", "iPhone 17", "iOS 26.5")
	if err != nil {
		t.Fatal(err)
	}
	orphan := strings.TrimSpace(string(out))
	out, err = run.Simctl(ctx, "create", proto.ImageDeviceNamePrefix+"img_boot", "iPhone 17", "iOS 26.5")
	if err != nil {
		t.Fatal(err)
	}
	booted := strings.TrimSpace(string(out))
	run.states[booted] = "Booted"
	out, err = run.Simctl(ctx, "create", "keeper", "iPhone 17", "iOS 26.5")
	if err != nil {
		t.Fatal(err)
	}
	keeper := strings.TrimSpace(string(out))

	s := NewImageStore(run, filepath.Join(base, "images"), nil)
	<-s.sweepDone
	if _, ok := run.states[orphan]; ok {
		t.Fatal("shutdown orphan manzanasd-img-* sim should be swept")
	}
	if _, ok := run.states[booted]; !ok {
		t.Fatal("booted sim must not be deleted by the sweep")
	}
	if _, ok := run.states[keeper]; !ok {
		t.Fatal("non-image sim must not be touched by the sweep")
	}
}

func TestImageStampRejectsTamperedArchive(t *testing.T) {
	s, _ := newTestStore(t, nil)
	ctx := context.Background()
	info, err := s.Build(ctx, buildReq())
	if err != nil {
		t.Fatal(err)
	}
	if info.SHA256 == "" {
		t.Fatal("build must record an archive digest")
	}
	if err := os.WriteFile(s.archivePath(info.ID), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Stamp(ctx, info.ID, 1, "qa"); !errors.Is(err, ErrImageCorrupt) {
		t.Fatalf("want ErrImageCorrupt, got %v", err)
	}
}

func TestImageBuildResolvesRuntimeDisplayName(t *testing.T) {
	s, run := newTestStore(t, nil)
	info, err := s.Build(context.Background(), buildReq())
	if err != nil {
		t.Fatal(err)
	}
	// The image metadata keeps the display name; simctl create must have
	// been given the resolved identifier.
	if info.Runtime != "iOS 26.5" {
		t.Fatalf("metadata runtime = %q, want display name", info.Runtime)
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	for _, c := range run.calls {
		if c[0] == "create" {
			if c[3] != "com.apple.CoreSimulator.SimRuntime.iOS-26-5" {
				t.Fatalf("create runtime = %q, want identifier", c[3])
			}
			return
		}
	}
	t.Fatalf("no create call observed; calls=%v", run.calls)
}

func TestImageBuildUnknownRuntimeIsBadRequest(t *testing.T) {
	s, run := newTestStore(t, nil)
	req := buildReq()
	req.Runtime = "iOS 99.9"
	if _, err := s.Build(context.Background(), req); !errors.Is(err, ErrBadImageRequest) {
		t.Fatalf("want ErrBadImageRequest, got %v", err)
	}
	// No builder sim may have been created for an unresolvable runtime.
	if left := run.udidsWithPrefix(proto.ImageDeviceNamePrefix); len(left) != 0 {
		t.Fatalf("builder sim created for unknown runtime: %v", left)
	}
}

func TestImageBuildInvalidRuntimeCreateErrorIsBadRequest(t *testing.T) {
	s, run := newTestStore(t, nil)
	run.mu.Lock()
	run.failOn["create"] = "Invalid runtime: com.apple.CoreSimulator.SimRuntime.iOS-26-5"
	run.mu.Unlock()
	if _, err := s.Build(context.Background(), buildReq()); !errors.Is(err, ErrBadImageRequest) {
		t.Fatalf("want ErrBadImageRequest, got %v", err)
	}
}

func TestImageStampRejectsSimBootedMidStamp(t *testing.T) {
	s, run := newTestStore(t, nil)
	ctx := context.Background()
	info, err := s.Build(ctx, buildReq())
	if err != nil {
		t.Fatal(err)
	}
	// Simulate another agent booting the hidden sim right after its data
	// dir is swapped: the pre-swap check passes, so only the commit-time
	// re-verify can catch it.
	run.onReplace = func(dst string) {
		udid := filepath.Base(filepath.Dir(dst))
		run.mu.Lock()
		run.states[udid] = "Booted"
		run.mu.Unlock()
	}
	if _, _, err := s.Stamp(ctx, info.ID, 2, "qa"); !errors.Is(err, ErrTargetBusy) {
		t.Fatalf("want ErrTargetBusy, got %v", err)
	}
	// Full rollback: no visible or hidden stamped sims left behind.
	if left := run.udidsWithPrefix("qa-"); len(left) != 0 {
		t.Fatalf("visible sims leaked after mid-stamp boot: %v", left)
	}
	if left := run.udidsWithPrefix(proto.ImageDeviceNamePrefix); len(left) != 0 {
		t.Fatalf("hidden sims leaked after mid-stamp boot: %v", left)
	}
}

func TestImageStampSkipsTakenNames(t *testing.T) {
	s, _ := newTestStore(t, nil)
	ctx := context.Background()
	info, err := s.Build(ctx, buildReq())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Stamp(ctx, info.ID, 2, "qa"); err != nil {
		t.Fatal(err)
	}
	_, created, err := s.Stamp(ctx, info.ID, 2, "qa")
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || created[0].Name != "qa-3" || created[1].Name != "qa-4" {
		t.Fatalf("second stamp should continue numbering, got %+v", created)
	}
}

func TestImageStampByName(t *testing.T) {
	s, _ := newTestStore(t, nil)
	ctx := context.Background()
	if _, err := s.Build(ctx, buildReq()); err != nil {
		t.Fatal(err)
	}
	_, created, err := s.Stamp(ctx, "clean", 1, "byname")
	if err != nil || len(created) != 1 {
		t.Fatalf("stamp by name: %v %+v", err, created)
	}
}

func TestImageStampValidation(t *testing.T) {
	s, _ := newTestStore(t, nil)
	ctx := context.Background()
	info, err := s.Build(ctx, buildReq())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Stamp(ctx, info.ID, 0, "x"); !errors.Is(err, ErrBadImageRequest) {
		t.Fatalf("count 0: want ErrBadImageRequest, got %v", err)
	}
	if _, _, err := s.Stamp(ctx, info.ID, maxStampCount+1, "x"); !errors.Is(err, ErrBadImageRequest) {
		t.Fatalf("count too high: want ErrBadImageRequest, got %v", err)
	}
	if _, _, err := s.Stamp(ctx, info.ID, 1, proto.SnapshotDeviceNamePrefix+"x"); !errors.Is(err, ErrBadImageRequest) {
		t.Fatalf("reserved prefix: want ErrBadImageRequest, got %v", err)
	}
	if _, _, err := s.Stamp(ctx, info.ID, 1, "bad/prefix"); !errors.Is(err, ErrBadImageRequest) {
		t.Fatalf("prefix with separator: want ErrBadImageRequest, got %v", err)
	}
	// "manzanasd-img" would generate hidden "manzanasd-img-<n>" names.
	if _, _, err := s.Stamp(ctx, info.ID, 1, strings.TrimSuffix(proto.ImageDeviceNamePrefix, "-")); !errors.Is(err, ErrBadImageRequest) {
		t.Fatalf("almost-reserved prefix: want ErrBadImageRequest, got %v", err)
	}
	if _, _, err := s.Stamp(ctx, "img_nope", 1, "x"); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("missing image: want ErrImageNotFound, got %v", err)
	}
	// A blank id must not resolve to an unnamed image.
	if _, _, err := s.Stamp(ctx, "", 1, "x"); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("empty id: want ErrImageNotFound, got %v", err)
	}
	if err := s.Delete(ctx, ""); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("empty id delete: want ErrImageNotFound, got %v", err)
	}
}

func TestImageStampPartialFailureCleansUp(t *testing.T) {
	s, run := newTestStore(t, nil)
	ctx := context.Background()
	info, err := s.Build(ctx, buildReq())
	if err != nil {
		t.Fatal(err)
	}
	// Fail every create from now on: the stamp must not leave sims behind.
	run.mu.Lock()
	run.failOn["create"] = "no space left on device"
	run.mu.Unlock()
	if _, _, err := s.Stamp(ctx, info.ID, 2, "qa"); err == nil {
		t.Fatal("expected error")
	}
	if left := run.udidsWithPrefix("qa-"); len(left) != 0 {
		t.Fatalf("stamped sims leaked after failure: %v", left)
	}
	if left := run.udidsWithPrefix(proto.ImageDeviceNamePrefix); len(left) != 0 {
		t.Fatalf("hidden stamped sims leaked after failure: %v", left)
	}
}

func TestImageBuildSlimCheckFailsFast(t *testing.T) {
	s, run := newTestStore(t, func(ctx context.Context, udid, profile string) error { return nil })
	s.SetSlimCheck(func(profile string) error { return errors.New("no such profile") })
	req := buildReq()
	req.SlimProfile = "typo"
	if _, err := s.Build(context.Background(), req); !errors.Is(err, ErrBadImageRequest) {
		t.Fatalf("want ErrBadImageRequest, got %v", err)
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.calls) != 0 {
		t.Fatalf("no simctl calls expected before profile validation, got %v", run.calls)
	}
}

func TestImageStampHiddenUntilProvisioned(t *testing.T) {
	s, run := newTestStore(t, nil)
	ctx := context.Background()
	info, err := s.Build(ctx, buildReq())
	if err != nil {
		t.Fatal(err)
	}
	_, created, err := s.Stamp(ctx, info.ID, 2, "qa")
	if err != nil {
		t.Fatal(err)
	}
	// Final names committed via rename; no hidden names remain.
	if left := run.udidsWithPrefix(proto.ImageDeviceNamePrefix); len(left) != 0 {
		t.Fatalf("hidden names left after stamp: %v", left)
	}
	// Every stamped sim was created under a hidden name and only renamed
	// after its data dir was replaced.
	run.mu.Lock()
	defer run.mu.Unlock()
	var sawHiddenCreate bool
	for _, c := range run.calls {
		if c[0] == "create" && strings.HasPrefix(c[1], proto.ImageDeviceNamePrefix+"stamp-") {
			sawHiddenCreate = true
		}
		if c[0] == "create" && strings.HasPrefix(c[1], "qa-") {
			t.Fatalf("stamped sim created under its visible name: %v", c)
		}
	}
	if !sawHiddenCreate {
		t.Fatalf("no hidden create observed; calls=%v", run.calls)
	}
	for _, c := range created {
		if run.names[c.UDID] != c.Name {
			t.Fatalf("sim %s name = %q, want %q", c.UDID, run.names[c.UDID], c.Name)
		}
	}
}

func TestImageDelete(t *testing.T) {
	s, _ := newTestStore(t, nil)
	ctx := context.Background()
	info, err := s.Build(ctx, buildReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.archivePath(info.ID)); !os.IsNotExist(err) {
		t.Fatalf("archive should be gone: %v", err)
	}
	if _, _, err := s.Stamp(ctx, info.ID, 1, "x"); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("stamp after delete: want ErrImageNotFound, got %v", err)
	}
	if err := s.Delete(ctx, info.ID); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("double delete: want ErrImageNotFound, got %v", err)
	}
}
