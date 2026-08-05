package wda

import (
	"context"
	"fmt"
	"strings"
)

// Launcher starts (and restarts) a WebDriverAgent runner for one device.
// Implementations spawn the runner and return; readiness is probed by the
// Supervisor against the WDA HTTP endpoint.
type Launcher interface {
	// Launch starts (or restarts) WDA on the device.
	Launch(ctx context.Context) error
	// Stop tears down anything Launch left running on the host.
	Stop()
	// String describes the launcher for logs.
	String() string
}

// ParseLauncher builds a Launcher from a --device-wda-launch spec:
//
//	devicectl:<runner-bundle-id>  launch a WebDriverAgentRunner already
//	                              installed on the device (one-time
//	                              xcodebuild signing/install), e.g.
//	                              devicectl:com.example.WebDriverAgentRunner.xctrunner
//	xctestrun:<path>              run `xcodebuild test-without-building`
//	                              against a .xctestrun produced by
//	                              `xcodebuild build-for-testing` on this host
func ParseLauncher(udid, spec string) (Launcher, error) {
	scheme, rest, ok := strings.Cut(spec, ":")
	if !ok || rest == "" {
		return nil, fmt.Errorf("invalid WDA launch spec %q (want devicectl:<bundle-id> or xctestrun:<path>)", spec)
	}
	switch scheme {
	case "devicectl":
		return NewDevicectlLauncher(udid, rest), nil
	case "xctestrun":
		return NewXCTestRunLauncher(udid, rest), nil
	default:
		return nil, fmt.Errorf("unknown WDA launch scheme %q in %q (want devicectl: or xctestrun:)", scheme, spec)
	}
}
