package actions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
	"github.com/BariBariGood/manzanas/internal/actions/wda"
	"github.com/BariBariGood/manzanas/internal/elementmap"
	"github.com/BariBariGood/manzanas/internal/imgutil"
	"github.com/BariBariGood/manzanas/internal/templates"
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
	// mu guards wda and kick: both are runtime-mutable so a device can
	// be attached to (or detached from) a running daemon (POST
	// /v0/devices, SIGHUP config reload).
	mu sync.RWMutex
	// wda maps device UDID -> WDA client; nil entries mean unconfigured.
	wda map[string]*wda.Client
	// mirror maps device UDID -> mirrord helper client for mirror-backed
	// devices (--device-backend <udid>=mirror). A device is either
	// WDA-backed or mirror-backed, never both.
	mirror map[string]*mirror.Client
	// mirrorInput maps device UDID -> whether mutating (input) mirror
	// actions are allowed. Fail-closed: a mirror-backed device absent
	// from this map (or mapped to false) is read-only — input kinds
	// return `forbidden` while observe/screenshot/wait_* stay allowed.
	mirrorInput map[string]bool
	// kick maps device UDID -> supervisor kick: called when a WDA action
	// fails so the supervisor re-probes (and relaunches) immediately
	// instead of waiting out its probe interval.
	kick map[string]func()
	// bundles maps device UDID -> last launched bundle id, the key the
	// resolver ladder's AX circuit breaker scopes its state with.
	bundles map[string]string
	// axBreak is the per-lease, per-bundle circuit breaker for the AX
	// resolver rung (see resolver.go).
	axBreak *axBreaker
	// templates is the cross-run template library save_template writes
	// to; nil means the library is not configured (the action returns
	// "unavailable").
	templates *templates.Store
	// emap is the element-map learning/fast-path wiring; nil disables
	// both the map rung and learning (see resolver_map.go).
	emap *elementMapper
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

// WithDeviceMirror registers mirror-backed devices, mapping device UDID
// -> mirrord helper client. The mirror is exclusive global state (one
// phone per Mac, window must be frontmost), so a daemon supports at most
// one mirror-backed device; the flag parser enforces that.
func WithDeviceMirror(clients map[string]*mirror.Client) DeviceOption {
	return func(b *DeviceBackend) {
		for udid, c := range clients {
			b.mirror[udid] = c
		}
	}
}

// WithDeviceMirrorInput opts mirror-backed devices into mutating (input)
// mirror actions. Without it the mirror is read-only (fail-closed).
func WithDeviceMirrorInput(allow map[string]bool) DeviceOption {
	return func(b *DeviceBackend) {
		for udid, a := range allow {
			b.mirrorInput[udid] = a
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

// SetMirror points a device at a mirrord helper socket (empty removes
// the mirror client, reverting the device to WDA-backed) and records
// whether mutating (input) mirror actions are allowed for it —
// fail-closed: allowInput=false makes the mirror read-only. Safe to
// call on a serving backend: runtime device attach rewires the backend
// without a restart. The config validator enforces at most one
// mirror-backed device (the mirror is exclusive global state).
func (b *DeviceBackend) SetMirror(udid, socket string, allowInput bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if socket == "" {
		delete(b.mirror, udid)
		delete(b.mirrorInput, udid)
		return
	}
	b.mirror[udid] = mirror.New(socket)
	b.mirrorInput[udid] = allowInput
}

// SetWDA points a device's WDA client at url (empty removes it), and
// installs the matching supervisor kick (nil clears it). Safe to call on
// a serving backend: runtime device attach rewires WDA without a restart.
func (b *DeviceBackend) SetWDA(udid, url string, kick func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if url == "" {
		delete(b.wda, udid)
		delete(b.kick, udid)
		return
	}
	b.wda[udid] = wda.New(url)
	if kick != nil {
		b.kick[udid] = kick
	} else {
		delete(b.kick, udid)
	}
}

// SetTemplateStore wires the template library save_template writes to;
// nil leaves the action "unavailable".
func (b *DeviceBackend) SetTemplateStore(st *templates.Store) { b.templates = st }

// SetElementMapStore wires the element map (learning + the resolver
// ladder's "map" fast-path rung); nil disables both.
func (b *DeviceBackend) SetElementMapStore(st *elementmap.Store) {
	if st == nil {
		b.emap = nil
		return
	}
	b.emap = &elementMapper{store: st, capture: b.captureClassed, bundle: b.launchedBundle}
}

// captureClassed implements the element map's classed capture: the
// mirror window capture ("mirror") when the device is mirror-backed,
// the WDA screenshot ("wda") otherwise.
func (b *DeviceBackend) captureClassed(ctx context.Context, udid string) ([]byte, string, error) {
	if m := b.mirrorFor(udid); m != nil {
		scr, err := m.Screenshot(ctx)
		if err != nil {
			return nil, "", err
		}
		return scr.PNG, "mirror", nil
	}
	c, err := b.wdaFor(udid)
	if err != nil {
		return nil, "", err
	}
	png, err := c.Screenshot(ctx)
	if err != nil {
		return nil, "", err
	}
	return png, "wda", nil
}

// learnElementMap / invalidateElementMap implement elementMapLearner
// (see composite.go).
func (b *DeviceBackend) learnElementMap(ctx context.Context, udid, name string, n *Node) {
	b.emap.learn(ctx, udid, name, n)
}

func (b *DeviceBackend) invalidateElementMap(udid, name string) {
	b.emap.invalidate(udid, name)
}

// NewDevice builds the physical-device actions backend.
func NewDevice(opts ...DeviceOption) *DeviceBackend {
	b := &DeviceBackend{
		runner:      ExecRunner{},
		xcrun:       "xcrun",
		wda:         map[string]*wda.Client{},
		mirror:      map[string]*mirror.Client{},
		mirrorInput: map[string]bool{},
		kick:        map[string]func(){},
		bundles:     map[string]string{},
		axBreak:     newAXBreaker(),
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
		"scroll_to_element": handleDeviceScrollToElement,
		"scroll_until":      handleDeviceScrollUntil,
		"scroll_collect":    handleDeviceScrollCollect,
		"wait_for_element":  handleDeviceWaitForElement,
		"wait_tree_stable":  handleDeviceWaitTreeStable,
		"assert_text":       handleDeviceAssertText,
		"assert_no_text":    handleDeviceAssertNoText,
		"assert_template":   handleDeviceAssertTemplate,
		"tap_template":      handleDeviceTapTemplate,
		"wait_for_stable":   handleDeviceWaitPixelStable,
		"pasteboard_get":    handleDevicePasteboardGet,
		"pasteboard_set":    handleDevicePasteboardSet,
		"observe":           handleDeviceObserve,
		"screenshot":        handleDeviceScreenshot,
		"save_template":     handleDeviceSaveTemplate,
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
	"audit":        true,
	// Wheel-scroll is a mirror-backend kind (routed via mirrorHandlers
	// before this check); WDA has no wheel-event equivalent.
	"scroll": true,
}

// Dispatch implements Backend.
func (b *DeviceBackend) Dispatch(ctx context.Context, udid string, req proto.ActionRequest) (proto.ActionResult, error) {
	h, ok := b.handlers[req.Kind]
	mirrorSelfVerifies := false
	if b.mirrorFor(udid) != nil {
		if mh, mok := mirrorHandlers[req.Kind]; mok {
			if mirrorInputKinds[req.Kind] && !b.mirrorInputAllowed(udid) {
				return proto.ActionResult{}, forbidden("mirror input is disabled; set allow_input=true in the devices config (or --device-mirror-allow-input) to allow %q on device %s", req.Kind, udid)
			}
			h, ok = mh, true
			// The mirrord helper verifies its own gestures (stampMirrorAck
			// writes verified/verify/verify_delta, and an unverified
			// gesture already fails via mirrorAckCheck), so the helper
			// owns `verified` on mirror-backed devices: wrapping the
			// handler in runVerified would clobber its verdict with a
			// pixel-delta one and spend two extra captures on the
			// serialized helper.
			mirrorSelfVerifies = verifyKinds[req.Kind]
		}
	}
	if !ok {
		if deviceUnimplementedKinds[req.Kind] {
			return proto.ActionResult{}, notImplemented("action kind %q is not implemented for physical devices", req.Kind)
		}
		return proto.ActionResult{}, badRequest("action kind %q is not supported on physical devices", req.Kind)
	}
	// Opt-in pixel verification around tap/swipe (see verify.go): the
	// capture path follows the backend (mirror window capture when
	// mirror-backed, WDA screenshot otherwise). Report-only.
	var vspec verifySpec
	if verifyKinds[req.Kind] {
		var err error
		if vspec, err = verifySpecFromPayload(req.Payload); err != nil {
			return proto.ActionResult{}, err
		}
		if vspec.enabled {
			if err := verifyPrecheck(req.Kind, req.Payload); err != nil {
				return proto.ActionResult{}, err
			}
		}
	}
	var res map[string]any
	var err error
	if vspec.enabled && !mirrorSelfVerifies {
		res, err = runVerified(ctx, vspec, b.captureFrame(udid), func() (map[string]any, error) {
			return h(ctx, b, udid, req.Payload)
		})
	} else {
		res, err = h(ctx, b, udid, req.Payload)
	}
	if err != nil {
		return proto.ActionResult{}, err
	}
	if vspec.enabled && mirrorSelfVerifies {
		// The helper only stamps a verdict when its own verification ran
		// (verify:"off", or a helper predating the field, stamps nothing
		// useful): a caller who asked verify:true must never get a plain
		// success with no verification answer, so degrade to verify_error
		// (report-only contract, same as a failed capture).
		if v, ok := res["verify"].(string); !ok || v == "off" {
			if res == nil {
				res = map[string]any{}
			}
			res["verify_error"] = "pixel verification unavailable: the mirrord helper's own gesture verification is off (or the helper predates it), and the daemon defers to the helper on mirror-backed devices"
		}
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
	b.mu.RLock()
	c, ok := b.wda[udid]
	b.mu.RUnlock()
	if ok && c != nil {
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
	b.noteLaunchedBundle(udid, bundleID)
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
	dur, held, err := durationMSField(p)
	if err != nil {
		return nil, err
	}
	if held {
		if err := c.TouchAndHold(ctx, x, y, time.Duration(dur)*time.Millisecond); err != nil {
			return nil, b.wdaFail(udid, "tap", err)
		}
		return map[string]any{"x": x, "y": y, "duration_ms": dur, "backend": "wda"}, nil
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
	format, err := observeFormat(p)
	if err != nil {
		return nil, err
	}
	filter, err := treeFilterFromPayload(p)
	if err != nil {
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
	shown, err := scopeSubtree(p, nodes, nil)
	if err != nil {
		return nil, err
	}
	scoped := len(shown) != len(nodes) || (len(shown) > 0 && shown[0] != nodes[0])
	if filter.active() {
		shown = filterNodes(shown, filter)
	}
	if shown == nil {
		shown = []*Node{}
	}
	res := map[string]any{
		"hash":    TreeHash(nodes),
		"backend": "wda",
	}
	if format == "compact" {
		res["format"] = "compact"
		res["tree_compact"] = compactTreeText(shown)
	} else {
		res["tree"] = shown
	}
	if filter.active() || scoped {
		res["total_elements"] = countNodes(nodes)
		res["returned_elements"] = countNodes(shown)
	}
	if includeRaw {
		res["raw"] = xml
		res["raw_format"] = "xcuitest-xml"
	}
	return res, nil
}

// pngMagic is the PNG file signature (WDA builds may answer other image
// types; see wda.Client.Screenshot).
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

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
	// WDA builds may answer a non-PNG image (e.g. image/jpeg), so the
	// default fast path only skips the transcode when the bytes already
	// match the requested format.
	if format != "png" || maxDim > 0 || !bytes.HasPrefix(img, pngMagic) {
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
	b.mu.RLock()
	k := b.kick[udid]
	b.mu.RUnlock()
	if k != nil && isTransientWDAError(err) {
		k()
	}
	return unavailable("wda %s failed: %v", op, err)
}
