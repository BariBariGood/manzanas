package actions

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
	"github.com/BariBariGood/manzanas/internal/imgutil"
)

// Mirror-backed HID, observe, and composite element actions for physical
// devices (--device-backend <udid>=mirror): macOS iPhone Mirroring driven
// by the mirrord GUI helper (CGEvents + screencapture + Vision OCR)
// instead of WebDriverAgent. Nothing here touches XCTest, so apps that
// kill WDA's accessibility snapshot (TikTok-class) stay drivable.
//
// Fidelity contract: there is no accessibility tree. observe returns
// OCR-derived text boxes and declares it (`backend: "mirror"`,
// `fidelity: "ocr"`); element predicates match on visible text only, and
// composite actions carry the same fidelity fields. Coordinates are
// pixels in the capture image — the space screenshot, observe frames,
// and tap/swipe input all share (NOT device points).

// mirrorFidelity is the observe/composite fidelity declaration for the
// mirror backend: OCR text boxes, not an accessibility tree.
const mirrorFidelity = "ocr"

// mirrorBackedKinds routes the action kinds whose mirror implementation
// differs from the WDA one. App lifecycle (install/launch/terminate) is
// devicectl either way and stays in the shared handler map.
var mirrorHandlers = map[string]deviceHandlerFunc{
	"tap":               handleMirrorTap,
	"swipe":             handleMirrorSwipe,
	"type":              handleMirrorType,
	"button":            handleMirrorButton,
	"observe":           handleMirrorObserve,
	"screenshot":        handleMirrorScreenshot,
	"tap_element":       handleMirrorTapElement,
	"type_into_element": handleMirrorTypeIntoElement,
	"scroll_to_element": handleMirrorScrollToElement,
	"wait_for_element":  handleMirrorWaitForElement,
	"wait_tree_stable":  handleMirrorWaitTreeStable,
	"pasteboard_get":    handleMirrorPasteboard,
	"pasteboard_set":    handleMirrorPasteboard,
}

// mirrorFor returns the mirror client for a device, or nil when the
// device is WDA-backed. mirror is runtime-mutable (POST /v0/devices,
// SIGHUP config reload), hence the lock.
func (b *DeviceBackend) mirrorFor(udid string) *mirror.Client {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.mirror[udid]
}

// mirrorClient is mirrorFor with an "unavailable" error when the device
// is no longer mirror-backed (a runtime config change can detach the
// mirror between dispatch and the handler running).
func (b *DeviceBackend) mirrorClient(udid string) (*mirror.Client, error) {
	if c := b.mirrorFor(udid); c != nil {
		return c, nil
	}
	return nil, unavailable("device %s is no longer mirror-backed (the devices config changed)", udid)
}

// mirrorFail maps a mirrord failure onto the protocol error shape with
// an actionable message: the blocked/no-window/not-running states need a
// human at the Mac (or the phone), so the error says exactly what to do.
func mirrorFail(op string, err error) error {
	var me *mirror.Error
	if errors.As(err, &me) {
		switch me.Code {
		case "not-running":
			return unavailable("mirror %s failed: iPhone Mirroring is not running on the host; open the iPhone Mirroring app and connect the phone (this is a physical/GUI step)", op)
		case "no-window":
			return unavailable("mirror %s failed: iPhone Mirroring has no phone window; connect the phone in the app", op)
		case "blocked":
			return unavailable("mirror %s failed: iPhone Mirroring is showing a connect/paused interstitial (%s); if it says 'iPhone in Use', lock the physical phone so mirroring can resume — unlocking the phone pauses mirroring", op, me.Message)
		case "untypable", "bad-request":
			return badRequest("mirror %s failed: %s", op, me.Message)
		}
		return unavailable("mirror %s failed: %s", op, me.Message)
	}
	return unavailable("mirror %s failed (is the mirrord helper LaunchAgent running in the GUI session?): %v", op, err)
}

func handleMirrorTap(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.mirrorClient(udid)
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
		return nil, mirrorFail("tap", err)
	}
	return map[string]any{"x": x, "y": y, "backend": "mirror"}, nil
}

func handleMirrorSwipe(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.mirrorClient(udid)
	if err != nil {
		return nil, err
	}
	var vals [4]float64
	for i, k := range []string{"start_x", "start_y", "end_x", "end_y"} {
		v, err := coordField(p, k)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	duration := 0.5
	if d, ok := p["duration_seconds"]; ok {
		dv, err := toNum(d)
		// The mirrord helper is single-threaded and sleeps for the whole
		// gesture: an unbounded duration wedges every later action.
		if err != nil || dv <= 0 || dv > 10 {
			return nil, badRequest("duration_seconds must be a positive number of at most 10")
		}
		duration = dv
	}
	if err := c.Swipe(ctx, vals[0], vals[1], vals[2], vals[3], time.Duration(duration*float64(time.Second))); err != nil {
		return nil, mirrorFail("swipe", err)
	}
	return map[string]any{"start_x": vals[0], "start_y": vals[1],
		"end_x": vals[2], "end_y": vals[3], "backend": "mirror"}, nil
}

func handleMirrorType(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.mirrorClient(udid)
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
	if err := validateMirrorTypeOpts(opts); err != nil {
		return nil, err
	}
	if err := c.Type(ctx, text); err != nil {
		return nil, mirrorFail("type", err)
	}
	return map[string]any{"typed_runes": len([]rune(text)), "backend": "mirror"}, nil
}

// validateMirrorTypeOpts rejects typing options the mirror backend cannot
// service, before any UI mutation.
func validateMirrorTypeOpts(opts typeOpts) error {
	if opts.strategy == typeStrategyPaste {
		return notImplemented("the %q typing strategy is simulator-only; the mirror backend types via raw HID keycodes through iPhone Mirroring", typeStrategyPaste)
	}
	if opts.requireFocus {
		return notImplemented("require_focus is unavailable on the mirror backend: OCR observation cannot see the on-screen keyboard's accessibility elements")
	}
	return nil
}

// mirrorButtons maps the protocol's button names onto iPhone Mirroring
// keyboard shortcuts. Lock/volume are physical-device functions the
// mirror cannot reach.
var mirrorButtons = map[string]string{
	"home": "cmd+1",
}

func handleMirrorButton(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.mirrorClient(udid)
	if err != nil {
		return nil, err
	}
	name, _ := p["name"].(string)
	combo, ok := mirrorButtons[name]
	if !ok {
		if name == "lock" || name == "volume-up" || name == "volume-down" {
			return nil, notImplemented("button %q is not reachable through iPhone Mirroring (only %q is; lock/volume are physical-phone functions)", name, "home")
		}
		return nil, badRequest("button on a mirror-backed device requires payload field %q, one of: home", "name")
	}
	if err := c.Press(ctx, combo); err != nil {
		return nil, mirrorFail("button", err)
	}
	return map[string]any{"button": name, "backend": "mirror"}, nil
}

func handleMirrorScreenshot(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.mirrorClient(udid)
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
	scr, err := c.Screenshot(ctx)
	if err != nil {
		return nil, mirrorFail("screenshot", err)
	}
	img := scr.PNG
	if format != "png" || maxDim > 0 {
		img, err = imgutil.Transcode(img, format, quality, maxDim)
		if err != nil {
			return nil, internal("screenshot transform failed: %v", err)
		}
	}
	sum := sha256.Sum256(img)
	// img_w/img_h are the pre-transform capture size — the tap/observe
	// coordinate space — so callers of size-capped screenshots can map
	// what they see back to tappable coordinates.
	return map[string]any{
		"format":           format,
		"bytes":            len(img),
		"sha256":           hex.EncodeToString(sum[:]),
		"backend":          "mirror",
		"img_w":            scr.ImgW,
		"img_h":            scr.ImgH,
		format + "_base64": base64.StdEncoding.EncodeToString(img),
	}, nil
}

func handleMirrorObserve(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	includeRaw, err := boolFlag(p, "include_raw", false)
	if err != nil {
		return nil, err
	}
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
	c, err := b.mirrorClient(udid)
	if err != nil {
		return nil, err
	}
	res, err := c.OCR(ctx)
	if err != nil {
		return nil, mirrorFail("observe", err)
	}
	nodes := ocrNodes(res.Boxes)
	viewport := &Frame{W: float64(res.ImgW), H: float64(res.ImgH)}
	shown, err := scopeSubtree(p, nodes, viewport)
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
	out := map[string]any{
		"hash":     TreeHash(nodes),
		"backend":  "mirror",
		"fidelity": mirrorFidelity,
		"img_w":    res.ImgW,
		"img_h":    res.ImgH,
		"note":     "OCR-derived text boxes from the iPhone Mirroring window, not an accessibility tree; coordinates are capture-image pixels",
	}
	if format == "compact" {
		out["format"] = "compact"
		out["tree_compact"] = compactTreeText(shown)
	} else {
		out["tree"] = shown
	}
	if filter.active() || scoped {
		out["total_elements"] = countNodes(nodes)
		out["returned_elements"] = countNodes(shown)
	}
	if includeRaw {
		out["raw"] = res.Boxes
		out["raw_format"] = "vision-ocr-boxes"
	}
	return out, nil
}

// ocrNodes converts OCR boxes into the shared compacted-node shape so the
// matcher, filters, and composite actions run unchanged. Every node is a
// flat "Text" leaf: predicates on role/id/placeholder/value cannot match
// (there is no tree to source them from) — text labels only.
func ocrNodes(boxes []mirror.OCRBox) []*Node {
	nodes := make([]*Node, 0, len(boxes))
	for _, o := range boxes {
		nodes = append(nodes, &Node{
			Role:         "Text",
			Label:        o.Text,
			Frame:        &Frame{X: o.X, Y: o.Y, W: o.W, H: o.H},
			Interactable: true,
		})
	}
	return nodes
}

// withMirrorFidelity stamps a composite action result with the mirror
// backend's fidelity declaration, so callers know element resolution was
// OCR text matching rather than accessibility-tree predicates.
func withMirrorFidelity(res map[string]any, err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	res["backend"] = "mirror"
	res["fidelity"] = mirrorFidelity
	return res, nil
}

func handleMirrorTapElement(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	return withMirrorFidelity(elemTap(ctx, b, udid, p))
}

func handleMirrorTypeIntoElement(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	// Reject unsupported typing options before the focusing tap runs:
	// elemTypeInto's own require_focus guard polls for keyboard elements
	// OCR can never see, so it must not get that far.
	opts, err := typeOptsFromPayload(p)
	if err != nil {
		return nil, err
	}
	if err := validateMirrorTypeOpts(opts); err != nil {
		return nil, err
	}
	return withMirrorFidelity(elemTypeInto(ctx, b, udid, p))
}

func handleMirrorScrollToElement(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	return withMirrorFidelity(elemScrollTo(ctx, b, udid, p))
}

func handleMirrorWaitForElement(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	return withMirrorFidelity(elemWaitFor(ctx, b, udid, p))
}

func handleMirrorWaitTreeStable(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	return withMirrorFidelity(elemTreeStable(ctx, b, udid, p))
}

func handleMirrorPasteboard(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	return nil, notImplemented("pasteboard access is unavailable on the mirror backend: iPhone Mirroring exposes no pasteboard channel (typing goes through raw HID keycodes)")
}

// mirrorObserveTransient reports mirrord failures a wait loop should ride
// out within its own budget: momentary capture hiccups while the window
// re-composites or gets re-activated. blocked/no-window stay hard
// failures — clearing them ("iPhone in Use", disconnected phone) takes a
// human at the phone, so waiting out the budget would only replace the
// actionable 503 with a misleading element-not-found timeout.
func mirrorObserveTransient(err error) bool {
	var me *mirror.Error
	return errors.As(err, &me) && me.Code == "capture-failed"
}

// mirrorObserveOnce implements the elementDriver observe poll for
// mirror-backed devices: one OCR pass over the window capture. Transient
// failures come back as errNotYet so wait loops keep polling, mirroring
// the WDA transport-blip behavior in observeOnce.
func (b *DeviceBackend) mirrorObserveOnce(ctx context.Context, udid string) (observation, error) {
	c, err := b.mirrorClient(udid)
	if err != nil {
		return observation{}, err
	}
	res, err := c.OCR(ctx)
	if err != nil {
		if mirrorObserveTransient(err) {
			return observation{}, errNotYet
		}
		return observation{}, mirrorFail("observe", err)
	}
	return observation{
		nodes:    ocrNodes(res.Boxes),
		viewport: &Frame{W: float64(res.ImgW), H: float64(res.ImgH)},
	}, nil
}
