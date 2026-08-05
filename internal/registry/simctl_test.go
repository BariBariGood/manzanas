package registry

import (
	"context"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

const sampleSimctl = `{
  "devices": {
    "com.apple.CoreSimulator.SimRuntime.iOS-26-5": [
      {
        "udid": "AAAA-1111",
        "name": "iPhone 17 Pro",
        "state": "Shutdown",
        "isAvailable": true,
        "deviceTypeIdentifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro"
      },
      {
        "udid": "BBBB-2222",
        "name": "Broken",
        "state": "Shutdown",
        "isAvailable": false,
        "deviceTypeIdentifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro"
      }
    ],
    "com.apple.CoreSimulator.SimRuntime.iOS-18-5": [
      {
        "udid": "CCCC-3333",
        "name": "iPhone 16",
        "state": "Booted",
        "isAvailable": true,
        "deviceTypeIdentifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-16"
      }
    ]
  }
}`

func TestSimctlListParsing(t *testing.T) {
	r := &SimctlRegistry{run: func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte(sampleSimctl), nil
	}}
	targets, err := r.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("got %d targets (unavailable should be skipped): %+v", len(targets), targets)
	}
	byUDID := map[string]proto.Target{}
	for _, tg := range targets {
		byUDID[tg.UDID] = tg
	}
	a := byUDID["AAAA-1111"]
	if a.Runtime != "iOS 26.5" || a.DeviceType != "iPhone 17 Pro" || a.State != proto.StateShutdown {
		t.Fatalf("a = %+v", a)
	}
	hasLabel := false
	for _, l := range a.Labels {
		if l == "iphone-17-pro" {
			hasLabel = true
		}
	}
	if !hasLabel {
		t.Fatalf("labels = %v", a.Labels)
	}
	c := byUDID["CCCC-3333"]
	if c.State != proto.StateBooted || c.Runtime != "iOS 18.5" {
		t.Fatalf("c = %+v", c)
	}
}
