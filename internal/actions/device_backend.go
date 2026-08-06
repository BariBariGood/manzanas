package actions

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/BariBariGood/manzanas/internal/actions/wda"
	"github.com/BariBariGood/manzanas/internal/imgutil"
	"github.com/BariBariGood/manzanas/proto"
)

// deviceHandlerFunc executes one action kind against a physical device.
type deviceHandlerFunc func(ctx context.Context, b *DeviceBackend, udid string, payload map[string]any) (map[string]any, error)

// DeviceBackend implements Backend for physical devices: app lifecycle via
// `xcrun devicectl` (through Runner, so tests run on Linux) and HID/observe/
// screenshot via a per-device WebDriverAgent HTTP endpoint. Actions that
// need WDA return "unavailable" until a WDA URL is configured for the
// device (--device-wda <udid>=<url>).
type DeviceBackend struct {
	runner   Runner
	xcrun    string
	handlers map[string]deviceHandlerFunc
	// wda maps device UDID -> WDA client; nil entries mean unconfigured.
	wda map[string]*wda.Client
	// kick maps device UDID -> supervisor kick: called when a WDA action
	// fails so the supervisor re-probes (and relaunches) immediately
	// instead of waiting out its probe interval.
	kick map[string]func()
}

// DeviceOption configures a DeviceBackend.
type DeviceOption func(*DeviceBackend)

// WithDeviceRunner swaps the command runner (tests).
func WithDeviceRunner(r Runner) DeviceOption { return func(b *DeviceBackend) { b.runner = r } }

// WithDeviceWDA registers WDA endpoints, mapping device UDID -> base URL
// (e.g. "http://127.0.0.1:8100").
func WithDeviceWDA(urls map[string]string) DeviceOption {
	return func(b *DeviceBackend) {
		for udid, u := range urls {
			b.wda[udid] = wda.New(u)
		}
	}
}

// WithDeviceWDAKick registers per-device supervisor kicks, invoked when a
// WDA-backed action fails so the supervisor restarts the runner promptly.
func WithDeviceWDAKick(kicks map[string]func()) DeviceOption {
	return func(b *DeviceBackend) {
		for udid, k := range kicks {
			b.kick[udid] = k
		}
	}
}

// NewDevice builds the physical-device actions backend.
func NewDevice(opts ...DeviceOption) *DeviceBackend {
	b := &DeviceBackend{
		runner: ExecRunner{},
		xcrun:  "xcrun",
		wda:    map[string]*wda.Client{},
		kick:   map[string]func(){},
	}
	for _, o := range opts {
		o(b)
	}
	b.handlers = map[string]deviceHandlerFunc{
		"install_app":       handleDeviceInstallApp,
		"launch_app":        handleDeviceLaunchApp,
		"terminate_app":     handleDeviceTerminateApp,
		"tap":               handleDeviceTap,
		"swipe":             handleDeviceSwipe,
		"type":              handleDeviceType,
		"button":            handleDeviceButton,
		"tap_element":       handleDeviceTapElement,
		"type_into_element": handleDeviceTypeIntoElement,
		"wait_for_element":  handleDeviceWaitForElement,
		"wait_tree_stable":  handleDeviceWaitTreeStable,
		"pasteboard_get":    handleDevicePasteboardGet,
		"pasteboard_set":    handleDevicePasteboardSet,
		"observe":           handleDeviceObserve,
		"screenshot":        handleDeviceScreenshot,
	}
	return b
}

// deviceUnimplementedKinds are simulator action kinds that have no WDA or
// devicectl equivalent yet; they surface as not_implemented (a capability
// gap) instead of bad_request (a caller error). Raw HID keycodes are an
// AXe/CoreSimulator concept WDA does not expose.
var deviceUnimplementedKinds = map[string]bool{
	"key":          true,
	"key_sequence": true,
}

// Dispatch implements Backend.
func (b *DeviceBackend) Dispatch(ctx context.Context, udid string, req proto.ActionRequest) (proto.ActionResult, error) {
	h, ok := b.handlers[req.Kind]
	if !ok {
		if deviceUnimplementedKinds[req.Kind] {
			return proto.ActionResult{}, notImplemented("action kind %q is not implemented for physical devices", req.Kind)
		}
		return proto.ActionResult{}, badRequest("action kind %q is not supported on physical devices", req.Kind)
	}
	res, err := h(ctx, b, udid, req.Payload)
	if err != nil {
		return proto.ActionResult{}, err
	}
	return proto.ActionResult{OK: true, Result: res}, nil
}

// devicectl runs `xcrun devicectl` with the given args, returning stdout.
func (b *DeviceBackend) devicectl(ctx context.Context, args ...string) ([]byte, error) {
	out, stderr, err := b.runner.Run(ctx, b.xcrun, append([]string{"devicectl"}, args...)...)
	if err != nil {
		if isDeviceDisconnected(stderr) {
			return nil, unavailable("device not connected: %s", firstLine(stderr))
		}
		return nil, internal("devicectl %s failed: %v: %s", args[0], err, trim(stderr))
	}
	return out, nil
}

// isDeviceDisconnected recognizes the CoreDevice errors for a paired but
// unreachable device, so an action against a disconnected phone surfaces a
// stable "unavailable" error instead of raw stderr.
func isDeviceDisconnected(stderr []byte) bool {
	s := strings.ToLower(string(stderr))
	return strings.Contains(s, "could not be established") ||
		strings.Contains(s, "device is not connected") ||
		strings.Contains(s, "tunnel connection could not be") ||
		strings.Contains(s, "failed to establish a connection") ||
		// A paired-but-unreachable phone surfaces as the DDI mount
		// failing (CoreDeviceError 12040) rather than an explicit
		// connection error.
		strings.Contains(s, "disk image could not be mounted") ||
		// A paired device whose tunnel is fully down is reported as
		// not found at all (CoreDeviceError 1011).
		strings.Contains(s, "unable to locate a device matching")
}

// wdaFor returns the WDA client for a device, or an "unavailable" error
// when none is configured.
func (b *DeviceBackend) wdaFor(udid string) (*wda.Client, error) {
	if c, ok := b.wda[udid]; ok && c != nil {
		return c, nil
	}
	return nil, unavailable("WebDriverAgent is not configured for device %s; start WDA on the device and pass --device-wda %s=<url>", udid, udid)
}

func handleDeviceInstallApp(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	path, _ := p["path"].(string)
	if path == "" {
		return nil, badRequest("install_app requires payload field %q (path to a .app bundle on the daemon host)", "path")
	}
	if _, err := b.devicectl(ctx, "device", "install", "app", "--device", udid, path); err != nil {
		return nil, err
	}
	return map[string]any{"installed": path}, nil
}

func handleDeviceLaunchApp(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	bundleID, _ := p["bundle_id"].(string)
	if bundleID == "" {
		return nil, badRequest("launch_app requires payload field %q", "bundle_id")
	}
	args := []string{"device", "process", "launch", "--device", udid}
	terminate, err := boolFlag(p, "terminate_running", false)
	if err != nil {
		return nil, err
	}
	if terminate {
		args = append(args, "--terminate-existing")
	}
	args = append(args, bundleID)
	if extra, ok := p["args"].([]any); ok {
		for _, a := range extra {
			s, ok := a.(string)
			if !ok {
				return nil, badRequest("launch_app args must be strings")
			}
			args = append(args, s)
		}
	}
	if _, err := b.devicectl(ctx, args...); err != nil {
		return nil, err
	}
	return map[string]any{"bundle_id": bundleID}, nil
}

func handleDeviceTerminateApp(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	// devicectl terminates by pid, not bundle id: the caller passes the
	// pid (launch_app on devices does not report one, but `devicectl
	// device info processes` lists them).
	v, ok := p["pid"]
	if !ok {
		return nil, badRequest("terminate_app on a physical device requires payload field %q (devicectl terminates by process id)", "pid")
	}
	n, err := toNum(v)
	if err != nil || n <= 0 || n != float64(int64(n)) {
		return nil, badRequest("payload field %q must be a positive integer", "pid")
	}
	pid := strconv.FormatInt(int64(n), 10)
	if _, err := b.devicectl(ctx, "device", "process", "terminate", "--device", udid, "--pid", pid); err != nil {
		return nil, err
	}
	return map[string]any{"terminated_pid": int64(n)}, nil
}

func handleDeviceTap(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.wdaFor(udid)
	if err != nil {
		return nil, err
	}
	x, err := coordField(p, "x")
	if err != nil {
		return nil, err
	}
	y, err := coordField(p, "y")
	if err != nil {
		return nil, err
	}
	if err := c.Tap(ctx, x, y); err != nil {
		return nil, b.wdaFail(udid, "tap", err)
	}
	return map[string]any{"x": x, "y": y, "backend": "wda"}, nil
}

func handleDeviceType(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.wdaFor(udid)
	if err != nil {
		return nil, err
	}
	text, ok := p["text"].(string)
	if !ok || text == "" {
		return nil, badRequest("type requires a non-empty string payload field %q", "text")
	}
	opts, err := typeOptsFromPayload(p)
	if err != nil {
		return nil, err
	}
	if err := b.validateTypeOpts(opts); err != nil {
		return nil, err
	}
	if opts.requireFocus {
		refresh, err := boolFlag(p, "refresh", false)
		if err != nil {
			return nil, err
		}
		if err := requireFocusedField(ctx, b, udid, refresh); err != nil {
			return nil, err
		}
	}
	if err := c.Keys(ctx, text); err != nil {
		return nil, b.wdaFail(udid, "type", err)
	}
	return map[string]any{"typed_runes": len([]rune(text)), "backend": "wda"}, nil
}

func handleDeviceObserve(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.wdaFor(udid)
	if err != nil {
		return nil, err
	}
	includeRaw, err := boolFlag(p, "include_raw", false)
	if err != nil {
		return nil, err
	}
	// refresh is a no-op on devices (every WDA source read is un-cached)
	// but is validated for payload consistency with the simulator kind.
	if _, err := boolFlag(p, "refresh", false); err != nil {
		return nil, err
	}
	xml, err := c.Source(ctx)
	if err != nil {
		return nil, b.wdaFail(udid, "observe", err)
	}
	nodes, err := CompactWDATree(xml)
	if err != nil {
		return nil, err
	}
	if nodes == nil {
		nodes = []*Node{}
	}
	res := map[string]any{
		"tree":    nodes,
		"hash":    TreeHash(nodes),
		"backend": "wda",
	}
	if includeRaw {
		res["raw"] = xml
		res["raw_format"] = "xcuitest-xml"
	}
	return res, nil
}

func handleDeviceScreenshot(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.wdaFor(udid)
	if err != nil {
		return nil, err
	}
	if _, err := boolFlag(p, "inline", true); err != nil {
		return nil, err
	}
	format, quality, maxDim, err := screenshotParams(p)
	if err != nil {
		return nil, err
	}
	img, err := c.Screenshot(ctx)
	if err != nil {
		return nil, b.wdaFail(udid, "screenshot", err)
	}
	if format != "png" || maxDim > 0 {
		img, err = imgutil.Transcode(img, format, quality, maxDim)
		if err != nil {
			return nil, internal("screenshot transform failed: %v", err)
		}
	}
	sum := sha256.Sum256(img)
	return map[string]any{
		"format":           format,
		"bytes":            len(img),
		"sha256":           hex.EncodeToString(sum[:]),
		"backend":          "wda",
		format + "_base64": base64.StdEncoding.EncodeToString(img),
	}, nil
}

// firstLine reduces devicectl's multi-line error dumps to their headline.
func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(s)
}

// wdaFail maps a WDA client failure onto the protocol error shape and, on
// transport-level failures (a dropped tunnel, a dead runner), kicks the
// device's WDA supervisor so it relaunches promptly.
func (b *DeviceBackend) wdaFail(udid, op string, err error) error {
	if k := b.kick[udid]; k != nil && isTransientWDAError(err) {
		k()
	}
	return unavailable("wda %s failed: %v", op, err)
}
