package warm

import (
	"context"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

func bootedSim(udid, name string) proto.Target {
	return proto.Target{
		UDID: udid, Kind: proto.TargetSimulator, Name: name,
		Runtime: "iOS 26.5", DeviceType: "iPhone 17", State: proto.StateBooted,
	}
}

// markIdlePast backdates the janitor's idle mark so a sweep reclaims
// without waiting out the real grace period.
func markIdlePast(p *Pool, udid string) {
	p.mu.Lock()
	p.idleSince[udid] = time.Now().Add(-2 * janitorIdleGrace)
	p.mu.Unlock()
}

func TestJanitorReclaimsIdleDaemonBootedSim(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock(bootedSim(udidA, "sim-a"))
	p := testPool(t, h, reg, AppleSiliconClass)

	p.markDaemonBooted(udidA)
	markIdlePast(p, udidA)
	p.sweepJanitor(context.Background())

	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateShutdown {
		t.Fatalf("daemon-booted idle sim not shut down: state %v", st)
	}
	if p.IsDaemonBooted(udidA) {
		t.Fatal("ownership record not dropped after reclaim")
	}
}

func TestJanitorWaitsOutIdleGrace(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock(bootedSim(udidA, "sim-a"))
	p := testPool(t, h, reg, AppleSiliconClass)

	p.markDaemonBooted(udidA)
	// First sweep only marks the sim idle; grace has not elapsed.
	p.sweepJanitor(context.Background())

	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatalf("sim reclaimed before the idle grace elapsed: state %v", st)
	}
}

func TestJanitorSkipsLeasedAndResetsIdleMark(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock(bootedSim(udidA, "sim-a"))
	p := testPool(t, h, reg, AppleSiliconClass)
	leased := true
	p.SetLeasedFunc(func(udid string) bool { return leased })

	p.markDaemonBooted(udidA)
	markIdlePast(p, udidA)
	p.sweepJanitor(context.Background())
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("leased sim was reclaimed")
	}

	// The lease cleared the idle mark: the next sweep must start the
	// grace over, not reclaim immediately.
	leased = false
	p.sweepJanitor(context.Background())
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("sim reclaimed without a fresh idle grace after its lease ended")
	}
}

func TestJanitorNeverTouchesUnmanagedSims(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock(bootedSim(udidA, "agent-scale-1"), bootedSim(udidB, "sim-b"))
	p := testPool(t, h, reg, AppleSiliconClass)

	// udidB is daemon-booted; udidA was booted by an agent outside the
	// daemon and must survive every sweep.
	p.markDaemonBooted(udidB)
	markIdlePast(p, udidB)
	p.sweepJanitor(context.Background())

	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("janitor shut down an unmanaged sim")
	}
	if st, _ := reg.Health(context.Background(), udidB); st != proto.StateShutdown {
		t.Fatal("janitor did not reclaim the daemon-booted sim")
	}
}

func TestJanitorSkipsStreamingAndRecording(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock(bootedSim(udidA, "sim-a"))
	p := testPool(t, h, reg, AppleSiliconClass)
	streaming := true
	p.SetStreamingFunc(func(udid string) bool { return streaming })

	p.markDaemonBooted(udidA)
	markIdlePast(p, udidA)
	p.sweepJanitor(context.Background())
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("streamed sim was reclaimed")
	}

	streaming = false
	p.SetRecordingFunc(func(udid string) bool { return true })
	markIdlePast(p, udidA)
	p.sweepJanitor(context.Background())
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("recording sim was reclaimed")
	}
}

func TestJanitorIgnoresPoolMembers(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock(bootedSim(udidA, "sim-a"))
	p := testPool(t, h, reg, AppleSiliconClass)

	p.mu.Lock()
	p.members[udidA] = true
	p.mu.Unlock()
	// markDaemonBooted must refuse to record a member.
	p.markDaemonBooted(udidA)
	markIdlePast(p, udidA)
	p.sweepJanitor(context.Background())

	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("janitor shut down a pool member")
	}
}

// A sim can join the pool after it entered the daemon-booted ledger (a
// client boot racing startup provisioning): the sweep must never reclaim
// a member, even one still in the ledger.
func TestJanitorSkipsSimThatJoinedPoolAfterBoot(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock(bootedSim(udidA, "sim-a"))
	p := testPool(t, h, reg, AppleSiliconClass)

	p.markDaemonBooted(udidA)
	p.mu.Lock()
	p.members[udidA] = true
	p.mu.Unlock()
	markIdlePast(p, udidA)
	p.sweepJanitor(context.Background())

	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("janitor reclaimed a sim that became a pool member after boot")
	}
}

// A leaseless operator boot (dash) earns the wake grace: the sweep must
// leave it alone while the grace holds.
func TestJanitorSkipsOperatorBootedSimUnderWakeGrace(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock(bootedSim(udidA, "sim-a"))
	p := testPool(t, h, reg, AppleSiliconClass)

	p.markDaemonBooted(udidA)
	p.MarkAwake(udidA)
	markIdlePast(p, udidA)
	p.sweepJanitor(context.Background())

	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("janitor reclaimed an operator-booted sim inside its wake grace")
	}
}

// The wake grace belongs to the boot, and the lease lifecycle owned that
// boot: lease end clears the grace so the sim is reclaimed promptly.
func TestOnLeaseEndClearsWakeGrace(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock(bootedSim(udidA, "sim-a"))
	p := testPool(t, h, reg, AppleSiliconClass)

	p.markDaemonBooted(udidA)
	p.MarkAwake(udidA)
	p.OnLeaseEnd(udidA)

	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateShutdown {
		t.Fatal("lease end did not reclaim a daemon-booted sim under wake grace")
	}
}

func TestOnLeaseEndReclaimsDaemonBootedSim(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock(bootedSim(udidA, "sim-a"))
	p := testPool(t, h, reg, AppleSiliconClass)

	p.markDaemonBooted(udidA)
	p.OnLeaseEnd(udidA)
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateShutdown {
		t.Fatalf("lease-end did not reclaim the daemon-booted sim: state %v", st)
	}
}

func TestOnLeaseEndLeavesUnmanagedAndMembersAlone(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock(bootedSim(udidA, "agent-scale-1"), bootedSim(udidB, "member"))
	p := testPool(t, h, reg, AppleSiliconClass)
	p.mu.Lock()
	p.members[udidB] = true
	p.mu.Unlock()

	p.OnLeaseEnd(udidA)
	p.OnLeaseEnd(udidB)
	for _, u := range []string{udidA, udidB} {
		if st, _ := reg.Health(context.Background(), u); st != proto.StateBooted {
			t.Fatalf("OnLeaseEnd shut down %s", u)
		}
	}
}

func TestOnLeaseEndSkipsWhenReLeased(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock(bootedSim(udidA, "sim-a"))
	p := testPool(t, h, reg, AppleSiliconClass)
	p.SetLeasedFunc(func(udid string) bool { return true })

	p.markDaemonBooted(udidA)
	p.OnLeaseEnd(udidA)
	if st, _ := reg.Health(context.Background(), udidA); st != proto.StateBooted {
		t.Fatal("OnLeaseEnd shut down a re-leased sim")
	}
}

func TestUnmanagedCountInStatus(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock(bootedSim(udidA, "agent-scale-1"), bootedSim(udidB, "sim-b"))
	p := testPool(t, h, reg, AppleSiliconClass)
	p.markDaemonBooted(udidB)

	st := p.Status(context.Background())
	if st.Running != 2 {
		t.Fatalf("running = %d, want 2", st.Running)
	}
	if st.Unmanaged != 1 {
		t.Fatalf("unmanaged = %d, want 1", st.Unmanaged)
	}
}
