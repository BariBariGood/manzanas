package runspec

import (
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

const fullSpec = `
name: login-smoke
target:
  labels: [ios26]
  runtime: iOS 26.5
  device_type: iPhone 17 Pro
  reset: none
  fixtures:
    - name: seed-user
      payload: {user: agent}
app:
  path: /tmp/Fake.app
  bundle_id: com.example.fake
  terminate_running: true
steps:
  - name: focus username
    action: tap_element
    with: {id: username}
  - action: type
    with: {text: agent}
  - action: wait_for_element
    with: {label: "Welcome, agent!", timeout_ms: 5000}
    timeout_seconds: 10
  - name: quality gate
    action: audit
    continue_on_error: true
  - action: screenshot
artifacts:
  final_screenshot: true
  export: true
timeouts:
  acquire_seconds: 30
  run_seconds: 120
  step_seconds: 15
`

func TestParseFullSpec(t *testing.T) {
	spec, err := Parse([]byte(fullSpec))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "login-smoke" || len(spec.Steps) != 5 {
		t.Fatalf("parsed %+v", spec)
	}
	if spec.Target.Runtime != "iOS 26.5" || spec.Target.DeviceType != "iPhone 17 Pro" {
		t.Fatalf("target %+v", spec.Target)
	}
	if spec.Target.Fixtures[0].Name != "seed-user" || spec.Target.Fixtures[0].Payload["user"] != "agent" {
		t.Fatalf("fixtures %+v", spec.Target.Fixtures)
	}
	if spec.App.Path != "/tmp/Fake.app" || spec.App.BundleID != "com.example.fake" || !spec.App.TerminateRunning {
		t.Fatalf("app %+v", spec.App)
	}
	if spec.Steps[2].TimeoutSeconds != 10 || !spec.Steps[3].ContinueOnError {
		t.Fatalf("steps %+v", spec.Steps)
	}
	if spec.Timeouts != (proto.RunTimeouts{AcquireSeconds: 30, RunSeconds: 120, StepSeconds: 15}) {
		t.Fatalf("timeouts %+v", spec.Timeouts)
	}
	if spec.Artifacts.FinalScreenshot == nil || !*spec.Artifacts.FinalScreenshot {
		t.Fatalf("artifacts %+v", spec.Artifacts)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte("target: {labels: [ios26]}\nstep:\n  - action: tap\n"))
	if err == nil || !strings.Contains(err.Error(), "step") {
		t.Fatalf("want unknown-field error, got %v", err)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string // substring of the expected error; "" = valid
	}{
		{"no target", "steps: [{action: tap}]", "target requires"},
		{"step without action", "target: {labels: [a]}\nsteps: [{name: x}]", "action is required"},
		{"action and maestro_flow", "target: {labels: [a]}\nsteps: [{action: tap, maestro_flow: f.yaml}]", "mutually exclusive"},
		{"empty app", "target: {labels: [a]}\napp: {launch: false}", "app requires"},
		{"maestro only is schema-valid", "target: {labels: [a]}\nsteps: [{maestro_flow: f.yaml}]", ""},
		{"udid only", "target: {udid: UDID-1}", ""},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(tc.yaml))
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}
