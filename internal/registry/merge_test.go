package registry

import (
	"context"
	"errors"
	"testing"

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
