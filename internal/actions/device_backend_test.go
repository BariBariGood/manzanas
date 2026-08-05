package actions

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// testDeviceBackend builds a DeviceBackend over the fake runner with no
// WDA configured.
func testDeviceBackend(f *fakeRunner) *DeviceBackend {
	return NewDevice(WithDeviceRunner(f))
}

func deviceDispatch(t *testing.T, b *DeviceBackend, kind string, payload map[string]any) (proto.ActionResult, error) {
	t.Helper()
	return b.Dispatch(context.Background(), "DEV-UDID", proto.ActionRequest{Kind: kind, Payload: payload})
}

func actionCode(t *testing.T, err error) string {
	t.Helper()
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("got %T %v, want *actions.Error", err, err)
	}
	return ae.Code
}

func TestDeviceInstallApp(t *testing.T) {
	f := newFakeRunner()
	b := testDeviceBackend(f)
	res, err := deviceDispatch(t, b, "install_app", map[string]any{"path": "/tmp/My.app"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["installed"] != "/tmp/My.app" {
		t.Fatalf("res = %+v", res)
	}
	want := "xcrun devicectl device install app --device DEV-UDID /tmp/My.app"
	if got := f.argvs(); len(got) != 1 || got[0] != want {
		t.Fatalf("calls = %v, want [%s]", got, want)
	}

	_, err = deviceDispatch(t, b, "install_app", nil)
	if actionCode(t, err) != "bad_request" {
		t.Fatalf("missing path: %v", err)
	}
}

func TestDeviceLaunchApp(t *testing.T) {
	f := newFakeRunner()
	b := testDeviceBackend(f)
	res, err := deviceDispatch(t, b, "launch_app",
		map[string]any{"bundle_id": "com.example.app", "terminate_running": true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["bundle_id"] != "com.example.app" {
		t.Fatalf("res = %+v", res)
	}
	want := "xcrun devicectl device process launch --device DEV-UDID --terminate-existing com.example.app"
	if got := f.argvs(); len(got) != 1 || got[0] != want {
		t.Fatalf("calls = %v, want [%s]", got, want)
	}
}

func TestDeviceLaunchAppDisconnectedError(t *testing.T) {
	f := newFakeRunner()
	f.errs["devicectl"] = "ERROR: A connection to this device could not be established. (com.apple.dt.CoreDeviceError error 1)"
	b := testDeviceBackend(f)
	_, err := deviceDispatch(t, b, "launch_app", map[string]any{"bundle_id": "com.example.app"})
	if actionCode(t, err) != "unavailable" {
		t.Fatalf("got %v, want unavailable", err)
	}
	if !strings.Contains(err.Error(), "device not connected") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeviceLaunchAppDeviceNotFoundError(t *testing.T) {
	f := newFakeRunner()
	f.errs["devicectl"] = "ERROR: CoreDeviceService was unable to locate a device matching the requested device identifier. (com.apple.dt.CoreDeviceError error 1011 (0x3F3))"
	b := testDeviceBackend(f)
	_, err := deviceDispatch(t, b, "launch_app", map[string]any{"bundle_id": "com.example.app"})
	if actionCode(t, err) != "unavailable" {
		t.Fatalf("got %v, want unavailable", err)
	}
	if !strings.Contains(err.Error(), "device not connected") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeviceTerminateApp(t *testing.T) {
	f := newFakeRunner()
	b := testDeviceBackend(f)
	res, err := deviceDispatch(t, b, "terminate_app", map[string]any{"pid": 1234})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result["terminated_pid"] != int64(1234) {
		t.Fatalf("res = %+v", res)
	}
	want := "xcrun devicectl device process terminate --device DEV-UDID --pid 1234"
	if got := f.argvs(); len(got) != 1 || got[0] != want {
		t.Fatalf("calls = %v, want [%s]", got, want)
	}

	_, err = deviceDispatch(t, b, "terminate_app", map[string]any{"bundle_id": "com.example.app"})
	if actionCode(t, err) != "bad_request" {
		t.Fatalf("terminate without pid: %v", err)
	}
}

func TestDeviceWDAActionsUnavailableUntilConfigured(t *testing.T) {
	b := testDeviceBackend(newFakeRunner())
	for kind, payload := range map[string]map[string]any{
		"tap":        {"x": 1, "y": 2},
		"type":       {"text": "hi"},
		"observe":    nil,
		"screenshot": nil,
	} {
		_, err := deviceDispatch(t, b, kind, payload)
		if actionCode(t, err) != "unavailable" {
			t.Fatalf("%s: got %v, want unavailable", kind, err)
		}
		if !strings.Contains(err.Error(), "--device-wda") {
			t.Fatalf("%s: error should point at --device-wda: %v", kind, err)
		}
	}
}

func TestDeviceUnknownKindRejected(t *testing.T) {
	b := testDeviceBackend(newFakeRunner())
	_, err := deviceDispatch(t, b, "definitely_not_a_kind", nil)
	if actionCode(t, err) != "bad_request" {
		t.Fatalf("got %v, want bad_request", err)
	}
}

func TestDeviceSimOnlyKindNotImplemented(t *testing.T) {
	b := testDeviceBackend(newFakeRunner())
	for _, kind := range []string{"key", "key_sequence"} {
		_, err := deviceDispatch(t, b, kind, map[string]any{"code": 40})
		if actionCode(t, err) != "not_implemented" {
			t.Fatalf("%s: got %v, want not_implemented", kind, err)
		}
	}
}

// recordingBackend records which backend a dispatch landed on.
type recordingBackend struct{ kinds []string }

func (r *recordingBackend) Dispatch(ctx context.Context, udid string, req proto.ActionRequest) (proto.ActionResult, error) {
	r.kinds = append(r.kinds, udid+"/"+req.Kind)
	return proto.ActionResult{OK: true}, nil
}

func TestKindRouterRoutesByTargetKind(t *testing.T) {
	reg := registry.NewMock(
		proto.Target{UDID: "SIM-1", Kind: proto.TargetSimulator, Name: "iPhone 17 Pro",
			Runtime: "iOS 26.5", DeviceType: "iPhone 17 Pro", State: proto.StateBooted},
		proto.Target{UDID: "DEV-1", Kind: proto.TargetDevice, Name: "phone",
			Runtime: "iOS 26.5", DeviceType: "iPhone 15 Pro", State: proto.StateUnknown},
	)
	sim := &recordingBackend{}
	dev := &recordingBackend{}
	r := NewKindRouter(reg, sim, dev)
	ctx := context.Background()

	if _, err := r.Dispatch(ctx, "SIM-1", proto.ActionRequest{Kind: "tap"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Dispatch(ctx, "DEV-1", proto.ActionRequest{Kind: "launch_app"}); err != nil {
		t.Fatal(err)
	}
	// Unknown targets fall back to the simulator backend.
	if _, err := r.Dispatch(ctx, "GHOST", proto.ActionRequest{Kind: "tap"}); err != nil {
		t.Fatal(err)
	}
	if len(sim.kinds) != 2 || sim.kinds[0] != "SIM-1/tap" || sim.kinds[1] != "GHOST/tap" {
		t.Fatalf("sim = %v", sim.kinds)
	}
	if len(dev.kinds) != 1 || dev.kinds[0] != "DEV-1/launch_app" {
		t.Fatalf("dev = %v", dev.kinds)
	}
}
