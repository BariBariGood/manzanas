package registry

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
	"github.com/BariBariGood/manzanas/proto"
)

func mergedFixture(t *testing.T) (*MergedRegistry, *MockRegistry) {
	t.Helper()
	sims := NewMock(
		proto.Target{UDID: "SIM-1", Kind: proto.TargetSimulator, Name: "iPhone 17 Pro",
			Runtime: "iOS 26.5", DeviceType: "iPhone 17 Pro", State: proto.StateShutdown},
	)
	return Merge(sims, fixtureDevicectl(t)), sims
}

func TestMergedListConcatenates(t *testing.T) {
	m, _ := mergedFixture(t)
	targets, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 {
		t.Fatalf("got %d targets: %+v", len(targets), targets)
	}
	if targets[0].Kind != proto.TargetSimulator || targets[1].Kind != proto.TargetDevice {
		t.Fatalf("order not preserved: %+v", targets)
	}
}

// failingRegistry always errors, standing in for a sick devicectl.
type failingRegistry struct{}

func (failingRegistry) List(ctx context.Context) ([]proto.Target, error) {
	return nil, errors.New("devicectl: boom")
}
func (failingRegistry) Get(ctx context.Context, udid string) (proto.Target, error) {
	return proto.Target{}, errors.New("devicectl: boom")
}
func (failingRegistry) Boot(ctx context.Context, udid string) error     { return errors.New("boom") }
func (failingRegistry) Shutdown(ctx context.Context, udid string) error { return errors.New("boom") }
func (failingRegistry) Health(ctx context.Context, udid string) (proto.TargetState, error) {
	return proto.StateUnknown, errors.New("boom")
}

func TestMergedListDegradesOnSubRegistryFailure(t *testing.T) {
	sims := NewMock(
		proto.Target{UDID: "SIM-1", Kind: proto.TargetSimulator, Name: "iPhone 17 Pro",
			Runtime: "iOS 26.5", DeviceType: "iPhone 17 Pro", State: proto.StateShutdown},
	)
	m := Merge(sims, failingRegistry{})
	targets, err := m.List(context.Background())
	if err != nil {
		t.Fatalf("one healthy registry should keep List working: %v", err)
	}
	if len(targets) != 1 || targets[0].UDID != "SIM-1" {
		t.Fatalf("targets = %+v", targets)
	}
	if _, err := Merge(failingRegistry{}, failingRegistry{}).List(context.Background()); err == nil {
		t.Fatal("all registries failing should surface an error")
	}
}

func TestMergedRoutesPerUDID(t *testing.T) {
	m, _ := mergedFixture(t)
	ctx := context.Background()

	if tg, err := m.Get(ctx, "SIM-1"); err != nil || tg.Kind != proto.TargetSimulator {
		t.Fatalf("sim get: %+v %v", tg, err)
	}
	if tg, err := m.Get(ctx, "00008130-000000000000001A"); err != nil || tg.Kind != proto.TargetDevice {
		t.Fatalf("device get: %+v %v", tg, err)
	}
	var nf *NotFoundError
	if _, err := m.Get(ctx, "nope"); !errors.As(err, &nf) {
		t.Fatalf("got %v, want NotFoundError", err)
	}

	// Boot routes: sims boot, devices return the typed error.
	if err := m.Boot(ctx, "SIM-1"); err != nil {
		t.Fatalf("sim boot: %v", err)
	}
	var be *DeviceBootError
	if err := m.Boot(ctx, "00008130-000000000000001A"); !errors.As(err, &be) {
		t.Fatalf("device boot: got %v, want DeviceBootError", err)
	}

	if st, err := m.Health(ctx, "00008027-000000000000002B"); err != nil || st != proto.StateBooted {
		t.Fatalf("device health: %q %v", st, err)
	}
}

// mirrorOverlayFixture merges the devicectl fixture with a mirror
// registry claiming the fixture's disconnected phone plus a UDID
// devicectl does not know.
func mirrorOverlayFixture(t *testing.T, states map[string]mirror.Status) *MergedRegistry {
	t.Helper()
	mr := NewMirror(func(ctx context.Context, socket string) (mirror.Status, error) {
		st, ok := states[socket]
		if !ok {
			return mirror.Status{}, errors.New("mirrord unreachable")
		}
		return st, nil
	})
	mr.SetMirrors(map[string]string{
		"00008130-000000000000001A": "/tmp/phone.sock",
		"MIRROR-ONLY":               "/tmp/only.sock",
	})
	return Merge(fixtureDevicectl(t), mr)
}

func TestMergedListOverlaysMirrorOntoDevicectl(t *testing.T) {
	m := mirrorOverlayFixture(t, map[string]mirror.Status{
		"/tmp/phone.sock": {State: "ready"},
		"/tmp/only.sock":  {State: "ready"},
	})
	targets, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 {
		t.Fatalf("got %d targets, want 3 (dedup by UDID): %+v", len(targets), targets)
	}
	byUDID := map[string]proto.Target{}
	for _, tg := range targets {
		byUDID[tg.UDID] = tg
	}

	// devicectl metadata wins; mirror supplies liveness. The phone's
	// tunnel is down in the fixture, so "disconnected" becomes
	// "no-tunnel" while the mirror's ready state makes it Booted.
	phone := byUDID["00008130-000000000000001A"]
	if phone.Name != "test-iphone" || phone.DeviceType != "iPhone 15 Pro" {
		t.Fatalf("devicectl metadata lost: %+v", phone)
	}
	if phone.State != proto.StateBooted {
		t.Fatalf("mirror liveness lost: state = %q", phone.State)
	}
	for _, want := range []string{MirrorLabel, NoTunnelLabel, "iphone-15-pro"} {
		if !slices.Contains(phone.Labels, want) {
			t.Fatalf("phone labels missing %q: %v", want, phone.Labels)
		}
	}
	for _, banned := range []string{DisconnectedLabel, NoDevicectlLabel} {
		if slices.Contains(phone.Labels, banned) {
			t.Fatalf("phone labels carry %q: %v", banned, phone.Labels)
		}
	}

	// A mirror UDID unknown to devicectl is synthesized.
	only := byUDID["MIRROR-ONLY"]
	if only.Kind != proto.TargetDevice || only.State != proto.StateBooted || only.Name != "iPhone (mirror)" {
		t.Fatalf("synthesized target = %+v", only)
	}
	if !slices.Contains(only.Labels, NoDevicectlLabel) {
		t.Fatalf("synthesized target missing %q: %v", NoDevicectlLabel, only.Labels)
	}

	// The connected iPad has no mirror entry and is untouched.
	pad := byUDID["00008027-000000000000002B"]
	if pad.State != proto.StateBooted || slices.Contains(pad.Labels, MirrorLabel) {
		t.Fatalf("non-mirror device changed: %+v", pad)
	}
}

func TestMergedListMirrorUnreachableStaysDisconnected(t *testing.T) {
	// mirrord unreachable: liveness pessimism wins even though the
	// synthesized entry still enumerates.
	m := mirrorOverlayFixture(t, map[string]mirror.Status{})
	targets, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byUDID := map[string]proto.Target{}
	for _, tg := range targets {
		byUDID[tg.UDID] = tg
	}
	phone := byUDID["00008130-000000000000001A"]
	if phone.State != proto.StateUnknown {
		t.Fatalf("unreachable mirror should leave state Unknown: %+v", phone)
	}
	for _, want := range []string{DisconnectedLabel, NoTunnelLabel, MirrorLabel} {
		if !slices.Contains(phone.Labels, want) {
			t.Fatalf("phone labels missing %q: %v", want, phone.Labels)
		}
	}
}

func TestMergedGetOverlaysMirror(t *testing.T) {
	m := mirrorOverlayFixture(t, map[string]mirror.Status{
		"/tmp/phone.sock": {State: "blocked"},
		"/tmp/only.sock":  {State: "ready"},
	})
	ctx := context.Background()
	tg, err := m.Get(ctx, "00008130-000000000000001A")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Name != "test-iphone" || tg.State != proto.StateBooted || !slices.Contains(tg.Labels, MirrorBlockedLabel) {
		t.Fatalf("Get overlay = %+v", tg)
	}
	if st, err := m.Health(ctx, "00008130-000000000000001A"); err != nil || st != proto.StateBooted {
		t.Fatalf("health = %q, %v", st, err)
	}
	// Boot still routes to the first owner (devicectl) and refuses.
	var be *DeviceBootError
	if err := m.Boot(ctx, "00008130-000000000000001A"); !errors.As(err, &be) {
		t.Fatalf("boot: got %v, want DeviceBootError", err)
	}
}

func TestMergedListAllFallibleFailingSurfacesError(t *testing.T) {
	// A supplemental registry (mirror: List never fails) must not hide a
	// total outage of every fallible source behind an empty success.
	mr := NewMirror(func(ctx context.Context, socket string) (mirror.Status, error) {
		return mirror.Status{State: "ready"}, nil
	})
	mr.SetMirrors(map[string]string{"MIRROR-ONLY": "/tmp/only.sock"})
	if _, err := Merge(failingRegistry{}, failingRegistry{}, mr).List(context.Background()); err == nil {
		t.Fatal("all fallible registries failing should surface an error")
	}
	// With one fallible source healthy, the merge degrades gracefully and
	// mirror targets still enumerate.
	sims := NewMock()
	ts, err := Merge(sims, failingRegistry{}, mr).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tg := range ts {
		if tg.UDID == "MIRROR-ONLY" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mirror target missing from degraded list: %+v", ts)
	}
}
