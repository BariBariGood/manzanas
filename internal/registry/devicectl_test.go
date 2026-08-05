package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

// fixtureDevicectl returns a DevicectlRegistry replaying the captured
// `devicectl list devices` JSON fixture.
func fixtureDevicectl(t *testing.T) *DevicectlRegistry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "devicectl_list.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &DevicectlRegistry{run: func(ctx context.Context, args ...string) ([]byte, error) {
		return data, nil
	}}
}

func TestDevicectlListParsing(t *testing.T) {
	r := fixtureDevicectl(t)
	targets, err := r.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("got %d targets (non-default visibilityClass should be skipped): %+v", len(targets), targets)
	}
	byUDID := map[string]proto.Target{}
	for _, tg := range targets {
		byUDID[tg.UDID] = tg
	}

	phone := byUDID["00008130-000000000000001A"]
	if phone.Kind != proto.TargetDevice {
		t.Fatalf("phone kind = %q", phone.Kind)
	}
	if phone.Name != "test-iphone" || phone.Runtime != "iOS 26.5.2" || phone.DeviceType != "iPhone 15 Pro" {
		t.Fatalf("phone = %+v", phone)
	}
	if phone.State != proto.StateUnknown {
		t.Fatalf("disconnected phone state = %q, want Unknown", phone.State)
	}
	for _, want := range []string{"device", "ios26", "ios26.5", "iphone-15-pro", DisconnectedLabel} {
		if !slices.Contains(phone.Labels, want) {
			t.Fatalf("phone labels missing %q: %v", want, phone.Labels)
		}
	}

	pad := byUDID["00008027-000000000000002B"]
	if pad.State != proto.StateBooted {
		t.Fatalf("connected ipad state = %q, want Booted", pad.State)
	}
	if slices.Contains(pad.Labels, DisconnectedLabel) {
		t.Fatalf("connected ipad should not carry %q: %v", DisconnectedLabel, pad.Labels)
	}
}

func TestDevicectlGet(t *testing.T) {
	r := fixtureDevicectl(t)
	tg, err := r.Get(context.Background(), "00008130-000000000000001A")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Name != "test-iphone" {
		t.Fatalf("got %+v", tg)
	}
	if _, err := r.Get(context.Background(), "nope"); err == nil {
		t.Fatal("expected not found")
	} else {
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("got %T %v, want NotFoundError", err, err)
		}
	}
}

func TestDevicectlBootReturnsTypedError(t *testing.T) {
	r := fixtureDevicectl(t)
	err := r.Boot(context.Background(), "00008130-000000000000001A")
	var be *DeviceBootError
	if !errors.As(err, &be) || be.UDID != "00008130-000000000000001A" {
		t.Fatalf("got %T %v, want DeviceBootError", err, err)
	}
	var nf *NotFoundError
	if err := r.Boot(context.Background(), "nope"); !errors.As(err, &nf) {
		t.Fatalf("boot of unknown udid: got %v, want NotFoundError", err)
	}
}

func TestDevicectlShutdownRefused(t *testing.T) {
	r := fixtureDevicectl(t)
	var ds *DeviceShutdownError
	if err := r.Shutdown(context.Background(), "00008130-000000000000001A"); !errors.As(err, &ds) {
		t.Fatalf("got %v, want DeviceShutdownError", err)
	}
	var nf *NotFoundError
	if err := r.Shutdown(context.Background(), "nope"); !errors.As(err, &nf) {
		t.Fatalf("got %v, want NotFoundError", err)
	}
}

func TestDevicectlHealthFromConnectionState(t *testing.T) {
	r := fixtureDevicectl(t)
	st, err := r.Health(context.Background(), "00008027-000000000000002B")
	if err != nil || st != proto.StateBooted {
		t.Fatalf("connected: got %q %v", st, err)
	}
	st, err = r.Health(context.Background(), "00008130-000000000000001A")
	if err != nil || st != proto.StateUnknown {
		t.Fatalf("disconnected: got %q %v", st, err)
	}
}
