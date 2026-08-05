package warm

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// testReapPool builds a pool with stale-lock reaping opted in against a
// per-test lock dir.
func testReapPool(t *testing.T, reg registry.Registry) (*Pool, string) {
	t.Helper()
	dir := t.TempDir()
	p := NewPool(newFakeHost(), reg, Config{
		Class:          AppleSiliconClass,
		DevicesDir:     t.TempDir(),
		Erase:          func(context.Context, string) error { return nil },
		ReapStaleLocks: true,
		LockDir:        dir,
		Logger:         slog.Default(),
	})
	return p, dir
}

// writeSimLock writes a lock-protocol file for the sim with the given
// timestamp, backdating the file mtime to match (freshness uses the
// newer of the two).
func writeSimLock(t *testing.T, dir, udid, content string, when time.Time) {
	t.Helper()
	path := filepath.Join(dir, "sim-"+udid)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func staleLockLine(when time.Time) string {
	return "agent-dead1234 " + when.UTC().Format(time.RFC3339) + " abandoned task\n"
}

// markStalePast backdates the janitor's stale-lock mark so a sweep reaps
// without waiting out the real grace period.
func markStalePast(p *Pool, udid string) {
	p.mu.Lock()
	p.staleSince[udid] = time.Now().Add(-2 * lockReapGrace)
	p.mu.Unlock()
}

func TestReapLeavesFreshLockAlone(t *testing.T) {
	reg := registry.NewMock(bootedSim(udidA, "agent-scale-1"))
	p, dir := testReapPool(t, reg)
	writeSimLock(t, dir, udidA, staleLockLine(time.Now()), time.Now())

	markStalePast(p, udidA) // even a backdated mark must not matter
	p.sweepJanitor(context.Background())

	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("sim with a fresh lock was reaped")
	}
	p.mu.Lock()
	_, marked := p.staleSince[udidA]
	p.mu.Unlock()
	if marked {
		t.Fatal("fresh lock did not clear the stale mark")
	}
}

func TestReapShutsDownStaleLockAfterGrace(t *testing.T) {
	reg := registry.NewMock(bootedSim(udidA, "agent-scale-1"))
	p, dir := testReapPool(t, reg)
	old := time.Now().Add(-3 * time.Hour)
	writeSimLock(t, dir, udidA, staleLockLine(old), old)

	// First sweep only starts the grace.
	p.sweepJanitor(context.Background())
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("sim reaped before the grace elapsed")
	}

	markStalePast(p, udidA)
	p.sweepJanitor(context.Background())
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateShutdown {
		t.Fatal("stale-locked sim not shut down after the grace")
	}
	if st := p.Status(context.Background()); st.ReapedStaleLocks != 1 {
		t.Fatalf("reaped count = %d, want 1", st.ReapedStaleLocks)
	}
}

func TestReapShutsDownLocklessSimAfterGrace(t *testing.T) {
	reg := registry.NewMock(bootedSim(udidA, "agent-scale-1"))
	p, _ := testReapPool(t, reg)

	p.sweepJanitor(context.Background())
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("lockless sim reaped before the grace elapsed")
	}

	markStalePast(p, udidA)
	p.sweepJanitor(context.Background())
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateShutdown {
		t.Fatal("lockless sim not shut down after the grace")
	}
}

func TestReapFailsClosedOnMalformedLock(t *testing.T) {
	reg := registry.NewMock(bootedSim(udidA, "agent-scale-1"))
	p, dir := testReapPool(t, reg)
	old := time.Now().Add(-3 * time.Hour)
	writeSimLock(t, dir, udidA, "not a valid lock line\n", old)

	markStalePast(p, udidA)
	p.sweepJanitor(context.Background())

	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("sim with a malformed lock was reaped (must fail closed)")
	}
}

func TestReapGraceResetsWhenSimReLockedMidGrace(t *testing.T) {
	reg := registry.NewMock(bootedSim(udidA, "agent-scale-1"))
	p, dir := testReapPool(t, reg)

	// No lock: the sweep starts the grace.
	p.sweepJanitor(context.Background())

	// An agent claims the sim mid-grace: the fresh lock clears the mark.
	writeSimLock(t, dir, udidA, staleLockLine(time.Now()), time.Now())
	p.sweepJanitor(context.Background())

	// The lock disappears again: the grace must start over, so a sweep
	// right after the disappearance still doesn't reap.
	if err := os.Remove(filepath.Join(dir, "sim-"+udidA)); err != nil {
		t.Fatal(err)
	}
	p.sweepJanitor(context.Background())
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("sim reaped without a fresh grace after being re-locked mid-grace")
	}
}

func TestReapNeverTouchesConfiguredPoolSimAwaitingAdoption(t *testing.T) {
	reg := registry.NewMock(bootedSim(udidA, "pool-sim-pending"))
	p, _ := testReapPool(t, reg)
	p.SetConfiguredPool([]string{udidA})

	markStalePast(p, udidA)
	p.sweepJanitor(context.Background())
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("configured pool sim reaped before adoption")
	}
	if p.reapUnmanaged(context.Background(), udidA) {
		t.Fatal("reapUnmanaged shut down a configured pool sim")
	}
}

func TestReapNeverTouchesPoolMembersOrDaemonBooted(t *testing.T) {
	reg := registry.NewMock(bootedSim(udidA, "member"), bootedSim(udidB, "daemon-booted"))
	p, _ := testReapPool(t, reg)
	p.mu.Lock()
	p.members[udidA] = true
	p.mu.Unlock()
	p.markDaemonBooted(udidB)

	markStalePast(p, udidA)
	markStalePast(p, udidB)
	// Neither is in the unmanaged set, so neither is even considered;
	// reapUnmanaged's own guards are exercised directly too.
	if p.reapUnmanaged(context.Background(), udidA) {
		t.Fatal("reapUnmanaged shut down a pool member")
	}
	if p.reapUnmanaged(context.Background(), udidB) {
		t.Fatal("reapUnmanaged shut down a daemon-booted sim")
	}
}

func TestReapDisabledByDefault(t *testing.T) {
	reg := registry.NewMock(bootedSim(udidA, "agent-scale-1"))
	p := testPool(t, newFakeHost(), reg, AppleSiliconClass)

	markStalePast(p, udidA)
	p.sweepJanitor(context.Background())
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("janitor reaped an unmanaged sim without opt-in")
	}
}

func TestReapGraceResetsWhileSimInActiveUse(t *testing.T) {
	reg := registry.NewMock(bootedSim(udidA, "agent-scale-1"))
	p, _ := testReapPool(t, reg)
	streaming := true
	p.SetStreamingFunc(func(udid string) bool { return streaming })

	// Lockless but streaming: no grace accumulates.
	markStalePast(p, udidA)
	p.sweepJanitor(context.Background())
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("streaming sim reaped")
	}

	// The stream ends: a full fresh grace must run before any reap.
	streaming = false
	p.sweepJanitor(context.Background())
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("sim reaped without a fresh grace after streaming ended")
	}
}

func TestReapSkipsSweepWhenLockDirMissing(t *testing.T) {
	reg := registry.NewMock(bootedSim(udidA, "agent-scale-1"))
	p, dir := testReapPool(t, reg)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	markStalePast(p, udidA)
	p.sweepJanitor(context.Background())
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("sim reaped while the lock dir itself was missing (must fail closed)")
	}
}

func TestSimLockStateFreshViaMtimeOnly(t *testing.T) {
	dir := t.TempDir()
	// Stale recorded timestamp but a fresh mtime (a bare touch): the
	// newer of the two wins, so the lock is fresh.
	old := time.Now().Add(-3 * time.Hour)
	path := filepath.Join(dir, "sim-"+udidA)
	if err := os.WriteFile(path, []byte(staleLockLine(old)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := simLockState(dir, udidA, time.Now()); got != lockFresh {
		t.Fatalf("lock state = %v, want fresh", got)
	}
}
