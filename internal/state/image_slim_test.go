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

// slimStore returns a store whose SlimFunc disables every fake daemon
// (what a real simslim run does).
func slimStore(t *testing.T) (*ImageStore, *fsFakeRunner) {
	t.Helper()
	var run *fsFakeRunner
	slim := func(ctx context.Context, udid, profile string) error {
		run.disableAll(udid)
		return nil
	}
	var s *ImageStore
	s, run = newTestStore(t, slim)
	return s, run
}

func slimBuildReq() proto.ImageBuildRequest {
	req := buildReq()
	req.SlimProfile = "agent-qa"
	return req
}

func TestImageBuildRecordsDisables(t *testing.T) {
	s, run := slimStore(t)
	info, err := s.Build(context.Background(), slimBuildReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(info.DisabledServices) != len(run.daemons) || info.DisabledCount != len(run.daemons) {
		t.Fatalf("disable set not recorded: %+v", info)
	}
}

func TestImageBuildSlimNoOpFails(t *testing.T) {
	// A SlimFunc that succeeds but disables nothing (what simslim does on
	// runtimes it doesn't support) must fail the build, not archive a
	// stock sim labelled slim.
	slim := func(ctx context.Context, udid, profile string) error { return nil }
	s, run := newTestStore(t, slim)
	if _, err := s.Build(context.Background(), slimBuildReq()); err == nil {
		t.Fatal("expected zero-disable slim to fail the build")
	}
	if left := run.udidsWithPrefix(proto.ImageDeviceNamePrefix); len(left) != 0 {
		t.Fatalf("builder sim leaked: %v", left)
	}
	if imgs, _ := s.List(context.Background()); len(imgs) != 0 {
		t.Fatalf("index should be empty, got %+v", imgs)
	}
}

func TestImageBuildSlimRefusesOldRuntimes(t *testing.T) {
	s, _ := slimStore(t)
	for _, rt := range []string{"iOS 17.2", "com.apple.CoreSimulator.SimRuntime.iOS-17-2"} {
		req := slimBuildReq()
		req.Runtime = rt
		if _, err := s.Build(context.Background(), req); !errors.Is(err, ErrBadImageRequest) {
			t.Fatalf("runtime %q: want ErrBadImageRequest, got %v", rt, err)
		}
	}
	// Unslimmed builds on old runtimes stay allowed.
	if err := slimRuntimeGuard("iOS 26.5"); err != nil {
		t.Fatalf("iOS 26.5 must pass the guard: %v", err)
	}
}

func TestStampReappliesDisables(t *testing.T) {
	s, run := slimStore(t)
	ctx := context.Background()
	info, err := s.Build(ctx, slimBuildReq())
	if err != nil {
		t.Fatal(err)
	}
	_, created, err := s.Stamp(ctx, info.ID, 2, "qa")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range created {
		// The data-dir swap does not carry launchctl disables (they are
		// keyed to the UDID outside the data dir), so the stamp flow must
		// have re-applied and verified them.
		have := run.disabledOn(c.UDID)
		for _, svc := range info.DisabledServices {
			if !have[svc] {
				t.Fatalf("stamped sim %s missing disable for %s", c.UDID, svc)
			}
		}
		run.mu.Lock()
		st := run.states[c.UDID]
		run.mu.Unlock()
		if st != "Shutdown" {
			t.Fatalf("stamped sim %s not shut down after re-slim: %s", c.UDID, st)
		}
		// The sim must be recorded for post-erase re-slim.
		if svcs, ok, err := s.slimmed.Lookup(c.UDID); err != nil || !ok || len(svcs) != len(info.DisabledServices) {
			t.Fatalf("slim registry entry missing for %s: %v %v %v", c.UDID, svcs, ok, err)
		}
	}
}

func TestStampRefusesLegacySlimImage(t *testing.T) {
	// A slim image built before disables were captured has slim_profile
	// set but no disabled_services; it cannot be verified, so stamping it
	// must fail (rebuild the image) rather than hand out unslimmed sims.
	s, run := slimStore(t)
	ctx := context.Background()
	info, err := s.Build(ctx, slimBuildReq())
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the index as a pre-capture build would have written it.
	f, err := s.idx.load()
	if err != nil {
		t.Fatal(err)
	}
	for i := range f.Images {
		f.Images[i].DisabledServices = nil
		f.Images[i].DisabledCount = 0
	}
	if err := s.idx.save(f); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Stamp(ctx, info.ID, 1, "qa"); !errors.Is(err, ErrImageCorrupt) {
		t.Fatalf("want ErrImageCorrupt, got %v", err)
	}
	if left := run.udidsWithPrefix("qa-"); len(left) != 0 {
		t.Fatalf("sims leaked: %v", left)
	}
}

func TestStampReslimFailureRollsBack(t *testing.T) {
	s, run := slimStore(t)
	ctx := context.Background()
	info, err := s.Build(ctx, slimBuildReq())
	if err != nil {
		t.Fatal(err)
	}
	// Every spawn (print-disabled/disable) fails: verification cannot
	// succeed, so the whole stamp must fail and leak nothing.
	run.mu.Lock()
	run.failOn["spawn"] = "launchctl unavailable"
	run.mu.Unlock()
	if _, _, err := s.Stamp(ctx, info.ID, 2, "qa"); err == nil {
		t.Fatal("expected stamp to fail when disables cannot be verified")
	}
	if left := run.udidsWithPrefix(proto.ImageDeviceNamePrefix); len(left) != 0 {
		t.Fatalf("hidden stamped sims leaked: %v", left)
	}
	if left := run.udidsWithPrefix("qa-"); len(left) != 0 {
		t.Fatalf("visible stamped sims leaked: %v", left)
	}
	// Rollback must not leave registry entries for the deleted sims.
	f, err := s.slimmed.load()
	if err != nil || len(f.Sims) != 0 {
		t.Fatalf("registry not empty after rollback: %v %v", f.Sims, err)
	}
}

// An empty device listing (transient CoreSimulator state) must not
// prune: it would wipe every recorded disable set, and the sims would
// silently come back un-slimmed after their next erase.
func TestSweepDoesNotPruneOnEmptyDeviceList(t *testing.T) {
	base := t.TempDir()
	run := newFSFakeRunner(filepath.Join(base, "devices"))
	if err := os.MkdirAll(run.base, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "images")
	r := newSlimRegistry(filepath.Join(dir, "slimmed.json"))
	if err := r.RecordBatch([]string{"AA"}, []string{"com.apple.apsd"}); err != nil {
		t.Fatal(err)
	}
	s := NewImageStore(run, dir, nil)
	<-s.sweepDone
	if _, ok, err := s.slimmed.Lookup("AA"); err != nil || !ok {
		t.Fatalf("registry entry pruned on empty device list: ok=%v err=%v", ok, err)
	}
}

func TestSlimRegistryPrune(t *testing.T) {
	r := newSlimRegistry(t.TempDir() + "/slimmed.json")
	if err := r.RecordBatch([]string{"AA", "BB"}, []string{"com.apple.apsd"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Prune(map[string]bool{"BB": true}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := r.Lookup("AA"); ok {
		t.Fatal("pruned entry still present")
	}
	if _, ok, _ := r.Lookup("BB"); !ok {
		t.Fatal("live entry pruned")
	}
}

func TestReapplySlimAfterErase(t *testing.T) {
	s, run := slimStore(t)
	ctx := context.Background()
	info, err := s.Build(ctx, slimBuildReq())
	if err != nil {
		t.Fatal(err)
	}
	_, created, err := s.Stamp(ctx, info.ID, 1, "qa")
	if err != nil {
		t.Fatal(err)
	}
	udid := created[0].UDID
	if _, err := run.Simctl(ctx, "erase", udid); err != nil {
		t.Fatal(err)
	}
	if len(run.disabledOn(udid)) != 0 {
		t.Fatal("erase should wipe the fake's disable config")
	}
	applied, err := s.ReapplySlim(ctx, udid)
	if err != nil || !applied {
		t.Fatalf("ReapplySlim: %v %v", applied, err)
	}
	have := run.disabledOn(udid)
	for _, svc := range info.DisabledServices {
		if !have[svc] {
			t.Fatalf("re-slim missing disable for %s", svc)
		}
	}
	run.mu.Lock()
	st := run.states[udid]
	run.mu.Unlock()
	if st != "Shutdown" {
		t.Fatalf("sim left %s after re-slim", st)
	}
	// A sim never stamped from a slim image is a no-op.
	if applied, err := s.ReapplySlim(ctx, "AAAAAAAA-0000-4000-8000-000000000099"); err != nil || applied {
		t.Fatalf("unknown sim: %v %v", applied, err)
	}
}

func TestBuildUsesSlimVerify(t *testing.T) {
	s, _ := slimStore(t)
	var calls []string
	s.SetSlimVerify(func(ctx context.Context, udid, profile string) error {
		calls = append(calls, profile)
		return nil
	})
	if _, err := s.Build(context.Background(), slimBuildReq()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "agent-qa" {
		t.Fatalf("verify calls: %v", calls)
	}
}

func TestBuildFailsOnSlimVerifyDrift(t *testing.T) {
	s, run := slimStore(t)
	s.SetSlimVerify(func(ctx context.Context, udid, profile string) error {
		return errors.New("drifted: com.apple.apsd enabled")
	})
	_, err := s.Build(context.Background(), slimBuildReq())
	if err == nil || !strings.Contains(err.Error(), "drifted") {
		t.Fatalf("want drift error, got %v", err)
	}
	if left := run.udidsWithPrefix(proto.ImageDeviceNamePrefix); len(left) != 0 {
		t.Fatalf("builder sim leaked: %v", left)
	}
	if imgs, _ := s.List(context.Background()); len(imgs) != 0 {
		t.Fatalf("index should be empty, got %+v", imgs)
	}
}

func TestStampUsesSlimVerify(t *testing.T) {
	s, run := slimStore(t)
	ctx := context.Background()
	info, err := s.Build(ctx, slimBuildReq())
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	s.SetSlimVerify(func(vctx context.Context, udid, profile string) error {
		if profile != info.SlimProfile {
			t.Fatalf("verify got profile %q, want %q", profile, info.SlimProfile)
		}
		// ensureDisabled must have re-applied the disables before the
		// exact-match check runs.
		have := run.disabledOn(udid)
		for _, svc := range info.DisabledServices {
			if !have[svc] {
				t.Fatalf("verify ran before disables re-applied on %s (missing %s)", udid, svc)
			}
		}
		calls++
		return nil
	})
	if _, created, err := s.Stamp(ctx, info.ID, 2, "qa"); err != nil || len(created) != 2 {
		t.Fatalf("stamp: %v %v", created, err)
	}
	if calls != 2 {
		t.Fatalf("verify ran %d times, want 2", calls)
	}
}

func TestStampSkipsVerifyWhenProfileUnresolvable(t *testing.T) {
	s, _ := slimStore(t)
	ctx := context.Background()
	info, err := s.Build(ctx, slimBuildReq())
	if err != nil {
		t.Fatal(err)
	}
	// The image was built on another host; its profile file is not
	// deployed here. The stamp must still succeed off the recorded
	// disable set, without invoking the exact-match verifier.
	s.SetSlimCheck(func(profile string) error {
		return errors.New("no such profile")
	})
	s.SetSlimVerify(func(vctx context.Context, udid, profile string) error {
		t.Fatal("verify must not run for an unresolvable profile")
		return nil
	})
	if _, created, err := s.Stamp(ctx, info.ID, 1, "qa"); err != nil || len(created) != 1 {
		t.Fatalf("stamp: %v %v", created, err)
	}
}

func TestStampRollsBackOnSlimVerifyDrift(t *testing.T) {
	s, run := slimStore(t)
	ctx := context.Background()
	info, err := s.Build(ctx, slimBuildReq())
	if err != nil {
		t.Fatal(err)
	}
	s.SetSlimVerify(func(vctx context.Context, udid, profile string) error {
		return errors.New("drifted: com.apple.diagnosticd enabled")
	})
	if _, _, err := s.Stamp(ctx, info.ID, 2, "qa"); err == nil {
		t.Fatal("expected stamp to fail on verify drift")
	}
	if left := run.udidsWithPrefix(proto.ImageDeviceNamePrefix); len(left) != 0 {
		t.Fatalf("hidden stamped sims leaked: %v", left)
	}
	if left := run.udidsWithPrefix("qa-"); len(left) != 0 {
		t.Fatalf("visible stamped sims leaked: %v", left)
	}
	f, err := s.slimmed.load()
	if err != nil || len(f.Sims) != 0 {
		t.Fatalf("registry not empty after rollback: %v %v", f.Sims, err)
	}
}

func TestEnginePostEraseHook(t *testing.T) {
	run := newFakeRunner()
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Shutdown"
	e := NewSimEngine(run, t.TempDir()+"/snapshots.json")
	var hooked []string
	e.SetPostErase(func(ctx context.Context, udid string) error {
		hooked = append(hooked, udid)
		return nil
	})
	ctx := context.Background()
	if err := e.Erase(ctx, "AAAAAAAA-0000-4000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	if err := e.Reset(ctx, "AAAAAAAA-0000-4000-8000-000000000001", ResetErase); err != nil {
		t.Fatal(err)
	}
	if len(hooked) != 2 {
		t.Fatalf("post-erase hook ran %d times, want 2", len(hooked))
	}
	// The hook must not run on a no-op reset.
	if err := e.Reset(ctx, "AAAAAAAA-0000-4000-8000-000000000001", ResetNone); err != nil {
		t.Fatal(err)
	}
	if len(hooked) != 2 {
		t.Fatal("post-erase hook ran on a non-erase reset")
	}
}
