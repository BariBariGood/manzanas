package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func newTestEngine(t *testing.T) (*SimEngine, *fakeRunner) {
	t.Helper()
	run := newFakeRunner()
	e := NewSimEngine(run, filepath.Join(t.TempDir(), "snapshots.json"))
	return e, run
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	e, run := newTestEngine(t)
	ctx := context.Background()
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Shutdown"
	run.data["AAAAAAAA-0000-4000-8000-000000000001"] = "golden"

	snap, err := e.Snapshot(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "clean")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.SourceUDID != "AAAAAAAA-0000-4000-8000-000000000001" || snap.Label != "clean" || snap.CloneUDID == "" {
		t.Fatalf("bad snapshot info: %+v", snap)
	}

	run.data["AAAAAAAA-0000-4000-8000-000000000001"] = "mutated"
	rebooted, err := e.Restore(ctx, "AAAAAAAA-0000-4000-8000-000000000001", snap.ID, false)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rebooted {
		t.Fatal("shutdown sim should not report rebooted")
	}
	if run.data["AAAAAAAA-0000-4000-8000-000000000001"] != "golden" {
		t.Fatalf("data not restored: %q", run.data["AAAAAAAA-0000-4000-8000-000000000001"])
	}
}

func TestSnapshotRefusesBooted(t *testing.T) {
	e, run := newTestEngine(t)
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Booted"
	if _, err := e.Snapshot(context.Background(), "AAAAAAAA-0000-4000-8000-000000000001", ""); !errors.Is(err, ErrTargetBusy) {
		t.Fatalf("want ErrTargetBusy, got %v", err)
	}
}

func TestRestoreBootedRequiresRebootFlag(t *testing.T) {
	e, run := newTestEngine(t)
	ctx := context.Background()
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Shutdown"
	run.data["AAAAAAAA-0000-4000-8000-000000000001"] = "golden"
	snap, err := e.Snapshot(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Booted"
	run.data["AAAAAAAA-0000-4000-8000-000000000001"] = "mutated"
	if _, err := e.Restore(ctx, "AAAAAAAA-0000-4000-8000-000000000001", snap.ID, false); !errors.Is(err, ErrTargetBusy) {
		t.Fatalf("want ErrTargetBusy, got %v", err)
	}

	rebooted, err := e.Restore(ctx, "AAAAAAAA-0000-4000-8000-000000000001", snap.ID, true)
	if err != nil {
		t.Fatalf("Restore with reboot: %v", err)
	}
	if !rebooted {
		t.Fatal("want rebooted=true")
	}
	if run.data["AAAAAAAA-0000-4000-8000-000000000001"] != "golden" {
		t.Fatalf("data not restored: %q", run.data["AAAAAAAA-0000-4000-8000-000000000001"])
	}
	if run.states["AAAAAAAA-0000-4000-8000-000000000001"] != "Booted" {
		t.Fatalf("sim should be booted again, got %s", run.states["AAAAAAAA-0000-4000-8000-000000000001"])
	}
}

func TestRestoreRejectsForeignSnapshot(t *testing.T) {
	e, run := newTestEngine(t)
	ctx := context.Background()
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Shutdown"
	run.states["AAAAAAAA-0000-4000-8000-000000000002"] = "Shutdown"
	snap, err := e.Snapshot(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Guardrail: a snapshot taken from AAAAAAAA-0000-4000-8000-000000000001 must never be restored onto AAAAAAAA-0000-4000-8000-000000000002.
	if _, err := e.Restore(ctx, "AAAAAAAA-0000-4000-8000-000000000002", snap.ID, false); err == nil {
		t.Fatal("want error restoring foreign snapshot")
	}
}

func TestResetRejectsForeignSnapshotID(t *testing.T) {
	e, run := newTestEngine(t)
	ctx := context.Background()
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Shutdown"
	run.data["AAAAAAAA-0000-4000-8000-000000000001"] = "sim1-secret"
	run.states["AAAAAAAA-0000-4000-8000-000000000002"] = "Shutdown"
	run.data["AAAAAAAA-0000-4000-8000-000000000002"] = "sim2"
	snap, err := e.Snapshot(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "")
	if err != nil {
		t.Fatal(err)
	}
	// Guardrail: a lease reset naming another target's snapshot ID must not
	// copy that target's data onto the leased sim.
	if err := e.Reset(ctx, "AAAAAAAA-0000-4000-8000-000000000002", "snapshot:"+snap.ID); err == nil {
		t.Fatal("want error resetting with foreign snapshot ID")
	}
	if run.data["AAAAAAAA-0000-4000-8000-000000000002"] == "sim1-secret" {
		t.Fatal("foreign data leaked onto AAAAAAAA-0000-4000-8000-000000000002")
	}
}

func TestRestoreByLabelPicksNewest(t *testing.T) {
	e, run := newTestEngine(t)
	ctx := context.Background()
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Shutdown"
	run.data["AAAAAAAA-0000-4000-8000-000000000001"] = "v1"
	if _, err := e.Snapshot(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "clean"); err != nil {
		t.Fatal(err)
	}
	run.data["AAAAAAAA-0000-4000-8000-000000000001"] = "v2"
	if _, err := e.Snapshot(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "clean"); err != nil {
		t.Fatal(err)
	}
	run.data["AAAAAAAA-0000-4000-8000-000000000001"] = "mutated"
	if _, err := e.Restore(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "clean", false); err != nil {
		t.Fatalf("Restore by label: %v", err)
	}
	if run.data["AAAAAAAA-0000-4000-8000-000000000001"] != "v2" {
		t.Fatalf("want newest labelled snapshot (v2), got %q", run.data["AAAAAAAA-0000-4000-8000-000000000001"])
	}
}

func TestListAndDeleteSnapshots(t *testing.T) {
	e, run := newTestEngine(t)
	ctx := context.Background()
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Shutdown"
	snap, err := e.Snapshot(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "clean")
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := e.ListSnapshots(ctx)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("ListSnapshots: %v %v", snaps, err)
	}
	if err := e.DeleteSnapshot(ctx, snap.ID); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if _, ok := run.states[snap.CloneUDID]; ok {
		t.Fatal("clone device not deleted")
	}
	snaps, _ = e.ListSnapshots(ctx)
	if len(snaps) != 0 {
		t.Fatalf("index not emptied: %v", snaps)
	}
	if err := e.DeleteSnapshot(ctx, snap.ID); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("want ErrSnapshotNotFound, got %v", err)
	}
}

func TestDeleteSnapshotKeepsIndexOnFailure(t *testing.T) {
	e, run := newTestEngine(t)
	ctx := context.Background()
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Shutdown"
	snap, err := e.Snapshot(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "")
	if err != nil {
		t.Fatal(err)
	}
	run.failOn["delete"] = "device is busy"
	if err := e.DeleteSnapshot(ctx, snap.ID); err == nil {
		t.Fatal("want simctl delete error")
	}
	// The index entry must survive so the clone can be retried, not leak.
	snaps, _ := e.ListSnapshots(ctx)
	if len(snaps) != 1 {
		t.Fatalf("index entry dropped despite failed delete: %v", snaps)
	}
	delete(run.failOn, "delete")
	if err := e.DeleteSnapshot(ctx, snap.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestRestoreAndResetMapMissingCloneToNotFound(t *testing.T) {
	e, run := newTestEngine(t)
	ctx := context.Background()
	udid := "AAAAAAAA-0000-4000-8000-000000000001"
	run.states[udid] = "Shutdown"
	snap, err := e.Snapshot(ctx, udid, "")
	if err != nil {
		t.Fatal(err)
	}
	// Clone deleted out of band: restore must answer not_found and a
	// snapshot reset must surface the same error (so ResetSink degrades
	// to erase instead of quarantining).
	delete(run.states, snap.CloneUDID)
	if _, err := e.Restore(ctx, udid, snap.ID, false); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("Restore: want ErrSnapshotNotFound, got %v", err)
	}
	if err := e.Reset(ctx, udid, "snapshot:"+snap.ID); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("Reset: want ErrSnapshotNotFound, got %v", err)
	}
}

func TestDeleteSnapshotReclaimsIndexWhenCloneGone(t *testing.T) {
	e, run := newTestEngine(t)
	ctx := context.Background()
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Shutdown"
	snap, err := e.Snapshot(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "")
	if err != nil {
		t.Fatal(err)
	}
	// Clone deleted out of band: the index entry must still be reclaimable.
	run.failOn["delete"] = "Invalid device: " + snap.CloneUDID
	if err := e.DeleteSnapshot(ctx, snap.ID); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	snaps, _ := e.ListSnapshots(ctx)
	if len(snaps) != 0 {
		t.Fatalf("index entry not reclaimed: %v", snaps)
	}
}

func TestEraseRequiresShutdown(t *testing.T) {
	e, run := newTestEngine(t)
	ctx := context.Background()
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Booted"
	if err := e.Erase(ctx, "AAAAAAAA-0000-4000-8000-000000000001"); !errors.Is(err, ErrTargetBusy) {
		t.Fatalf("want ErrTargetBusy, got %v", err)
	}
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Shutdown"
	if err := e.Erase(ctx, "AAAAAAAA-0000-4000-8000-000000000001"); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if run.data["AAAAAAAA-0000-4000-8000-000000000001"] != "erased" {
		t.Fatal("erase not applied")
	}
}

func TestResetErase(t *testing.T) {
	e, run := newTestEngine(t)
	ctx := context.Background()
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Booted"
	run.data["AAAAAAAA-0000-4000-8000-000000000001"] = "dirty"
	if err := e.Reset(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "erase"); err != nil {
		t.Fatalf("Reset erase: %v", err)
	}
	if run.states["AAAAAAAA-0000-4000-8000-000000000001"] != "Shutdown" || run.data["AAAAAAAA-0000-4000-8000-000000000001"] != "erased" {
		t.Fatalf("want shutdown+erased, got %s %q", run.states["AAAAAAAA-0000-4000-8000-000000000001"], run.data["AAAAAAAA-0000-4000-8000-000000000001"])
	}
}

func TestResetSnapshot(t *testing.T) {
	e, run := newTestEngine(t)
	ctx := context.Background()
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Shutdown"
	run.data["AAAAAAAA-0000-4000-8000-000000000001"] = "golden"
	if _, err := e.Snapshot(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "clean"); err != nil {
		t.Fatal(err)
	}
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Booted"
	run.data["AAAAAAAA-0000-4000-8000-000000000001"] = "dirty"
	if err := e.Reset(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "snapshot:clean"); err != nil {
		t.Fatalf("Reset snapshot: %v", err)
	}
	if run.states["AAAAAAAA-0000-4000-8000-000000000001"] != "Shutdown" || run.data["AAAAAAAA-0000-4000-8000-000000000001"] != "golden" {
		t.Fatalf("want shutdown+golden, got %s %q", run.states["AAAAAAAA-0000-4000-8000-000000000001"], run.data["AAAAAAAA-0000-4000-8000-000000000001"])
	}
}

func TestResetNoneAndInvalid(t *testing.T) {
	e, run := newTestEngine(t)
	ctx := context.Background()
	run.states["AAAAAAAA-0000-4000-8000-000000000001"] = "Booted"
	if err := e.Reset(ctx, "AAAAAAAA-0000-4000-8000-000000000001", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.Reset(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "none"); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("no-op resets must not call simctl: %v", run.callStrings())
	}
	if err := e.Reset(ctx, "AAAAAAAA-0000-4000-8000-000000000001", "bogus"); !errors.Is(err, ErrBadReset) {
		t.Fatalf("want ErrBadReset, got %v", err)
	}
}

func TestValidResetSpec(t *testing.T) {
	for _, ok := range []string{"", "none", "erase", "snapshot:clean"} {
		if !ValidResetSpec(ok) {
			t.Errorf("ValidResetSpec(%q) = false", ok)
		}
	}
	for _, bad := range []string{"bogus", "snapshot:", "SNAPSHOT:x"} {
		if ValidResetSpec(bad) {
			t.Errorf("ValidResetSpec(%q) = true", bad)
		}
	}
}
