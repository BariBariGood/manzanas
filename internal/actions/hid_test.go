package actions

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

const udid = "TEST-UDID"

func dispatch(t *testing.T, b *AXeBackend, kind string, payload map[string]any) (proto.ActionResult, error) {
	t.Helper()
	return b.Dispatch(context.Background(), udid, proto.ActionRequest{Kind: kind, Payload: payload})
}

func TestHIDArgv(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		payload map[string]any
		want    string
	}{
		{
			name:    "tap",
			kind:    "tap",
			payload: map[string]any{"x": 100.0, "y": 250.5},
			want:    "/fake/axe tap -x 100 -y 250.5 --udid TEST-UDID",
		},
		{
			name:    "swipe with duration",
			kind:    "swipe",
			payload: map[string]any{"start_x": 10.0, "start_y": 600.0, "end_x": 10.0, "end_y": 100.0, "duration_seconds": 0.5},
			want:    "/fake/axe swipe --start-x 10 --start-y 600 --end-x 10 --end-y 100 --duration 0.5 --udid TEST-UDID",
		},
		{
			name:    "type",
			kind:    "type",
			payload: map[string]any{"text": "hello world"},
			want:    "/fake/axe type hello world --udid TEST-UDID",
		},
		{
			name:    "button",
			kind:    "button",
			payload: map[string]any{"name": "home"},
			want:    "/fake/axe button home --udid TEST-UDID",
		},
		{
			name:    "key",
			kind:    "key",
			payload: map[string]any{"keycode": 40.0},
			want:    "/fake/axe key 40 --udid TEST-UDID",
		},
		{
			name:    "key sequence",
			kind:    "key_sequence",
			payload: map[string]any{"keycodes": []any{4.0, 5.0, 6.0}},
			want:    "/fake/axe key-sequence --keycodes 4,5,6 --udid TEST-UDID",
		},
		{
			name:    "launch app",
			kind:    "launch_app",
			payload: map[string]any{"bundle_id": "com.apple.Preferences", "terminate_running": true},
			want:    "xcrun simctl launch --terminate-running-process TEST-UDID com.apple.Preferences",
		},
		{
			name:    "install app",
			kind:    "install_app",
			payload: map[string]any{"path": "/tmp/My.app"},
			want:    "xcrun simctl install TEST-UDID /tmp/My.app",
		},
		{
			name:    "terminate app",
			kind:    "terminate_app",
			payload: map[string]any{"bundle_id": "com.apple.Preferences"},
			want:    "xcrun simctl terminate TEST-UDID com.apple.Preferences",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRunner()
			res, err := dispatch(t, testBackend(f), tc.kind, tc.payload)
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			if !res.OK {
				t.Fatalf("result not OK: %+v", res)
			}
			got := f.argvs()
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("argv = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDispatchValidation(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		payload map[string]any
		code    string
	}{
		{"unknown kind", "teleport", nil, proto.ErrBadRequest},
		{"tap missing y", "tap", map[string]any{"x": 1.0}, proto.ErrBadRequest},
		{"tap non-numeric", "tap", map[string]any{"x": "left", "y": 1.0}, proto.ErrBadRequest},
		{"empty text", "type", map[string]any{"text": ""}, proto.ErrBadRequest},
		{"bad button", "button", map[string]any{"name": "eject"}, proto.ErrBadRequest},
		{"unsupported volume button", "button", map[string]any{"name": "volume-up"}, proto.ErrBadRequest},
		{"negative tap x", "tap", map[string]any{"x": -5.0, "y": 10.0}, proto.ErrBadRequest},
		{"negative swipe end_y", "swipe", map[string]any{"start_x": 1.0, "start_y": 1.0, "end_x": 1.0, "end_y": -1.0}, proto.ErrBadRequest},
		{"non-bool terminate_running", "launch_app", map[string]any{"bundle_id": "com.x", "terminate_running": "yes"}, proto.ErrBadRequest},
		{"empty keycodes", "key_sequence", map[string]any{"keycodes": []any{}}, proto.ErrBadRequest},
		{"negative keycode", "key", map[string]any{"keycode": -1.0}, proto.ErrBadRequest},
		{"oversized keycode", "key", map[string]any{"keycode": float64(math.MaxUint32) + 1}, proto.ErrBadRequest},
		{"fractional keycode", "key", map[string]any{"keycode": 4.5}, proto.ErrBadRequest},
		{"negative keycode in sequence", "key_sequence", map[string]any{"keycodes": []any{4.0, -1.0}}, proto.ErrBadRequest},
		{"launch without bundle id", "launch_app", map[string]any{}, proto.ErrBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRunner()
			_, err := dispatch(t, testBackend(f), tc.kind, tc.payload)
			var ae *Error
			if !errors.As(err, &ae) {
				t.Fatalf("want *actions.Error, got %v", err)
			}
			if ae.Code != tc.code {
				t.Fatalf("code = %q, want %q", ae.Code, tc.code)
			}
			if len(f.argvs()) != 0 {
				t.Fatalf("invalid request still ran commands: %q", f.argvs())
			}
		})
	}
}

func TestMissingAXeIsUnavailable(t *testing.T) {
	f := newFakeRunner()
	b := NewAXe(WithRunner(f), WithAXePath(""))
	if b.AXeAvailable() {
		t.Fatal("AXeAvailable should be false")
	}
	_, err := dispatch(t, b, "tap", map[string]any{"x": 1.0, "y": 2.0})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != proto.ErrUnavailable {
		t.Fatalf("want unavailable error, got %v", err)
	}
	if !strings.Contains(ae.Message, "axe binary not found") {
		t.Fatalf("unhelpful message: %q", ae.Message)
	}
}

func TestTypeUnmappableCharIsBadRequest(t *testing.T) {
	f := newFakeRunner()
	f.errs["type"] = "Error: No keycode found for character: '\t'"
	_, err := dispatch(t, testBackend(f), "type", map[string]any{"text": "a\tb"})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != proto.ErrBadRequest {
		t.Fatalf("want bad_request for unmappable character, got %v", err)
	}
	if !strings.Contains(ae.Message, "may have been typed") {
		t.Fatalf("partial-effect caveat missing: %q", ae.Message)
	}
}

func TestShutdownTargetIsTargetNotBooted(t *testing.T) {
	f := newFakeRunner()
	f.errs["tap"] = "Cannot run accessibility commands against TEST-UDID. Current state: Shutdown"
	_, err := dispatch(t, testBackend(f), "tap", map[string]any{"x": 1.0, "y": 2.0})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != proto.ErrTargetNotBooted {
		t.Fatalf("want target_not_booted, got %v", err)
	}

	f = newFakeRunner()
	f.errs["launch"] = "Unable to lookup in current state: Shutdown"
	_, err = dispatch(t, testBackend(f), "launch_app", map[string]any{"bundle_id": "com.x"})
	if !errors.As(err, &ae) || ae.Code != proto.ErrTargetNotBooted {
		t.Fatalf("want target_not_booted from simctl, got %v", err)
	}
}

func TestLaunchAppPidIsNumber(t *testing.T) {
	f := newFakeRunner()
	f.stdout["launch"] = "com.apple.Preferences: 87937\n"
	res, err := dispatch(t, testBackend(f), "launch_app", map[string]any{"bundle_id": "com.apple.Preferences"})
	if err != nil {
		t.Fatalf("launch_app: %v", err)
	}
	if pid, ok := res.Result["pid"].(int); !ok || pid != 87937 {
		t.Fatalf("pid = %#v, want int 87937", res.Result["pid"])
	}
}

func TestCommandFailureIsInternal(t *testing.T) {
	f := newFakeRunner()
	f.errs["tap"] = "simulator not booted"
	_, err := dispatch(t, testBackend(f), "tap", map[string]any{"x": 1.0, "y": 2.0})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != proto.ErrInternal {
		t.Fatalf("want internal error, got %v", err)
	}
	if !strings.Contains(ae.Message, "simulator not booted") {
		t.Fatalf("stderr not surfaced: %q", ae.Message)
	}
}
