package actions

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/BariBariGood/manzanas/internal/actions/wda"
)

// WDA-backed HID, pasteboard, and composite element actions for physical
// devices. Everything here needs a configured (and reachable) WDA
// endpoint; app lifecycle stays in device_backend.go on devicectl.

// deviceButtons maps the protocol's button names onto WDA's pressButton
// names. "lock" goes through WDA's dedicated /wda/lock endpoint instead.
var deviceButtons = map[string]string{
	"home":        "home",
	"volume-up":   "volumeUp",
	"volume-down": "volumeDown",
}

func deviceButtonNames() string {
	names := []string{"lock"}
	for n := range deviceButtons {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func handleDeviceSwipe(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.wdaFor(udid)
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
		if err != nil || dv <= 0 {
			return nil, badRequest("duration_seconds must be a positive number")
		}
		duration = dv
	}
	if err := c.Swipe(ctx, vals[0], vals[1], vals[2], vals[3], duration); err != nil {
		return nil, b.wdaFail(udid, "swipe", err)
	}
	return map[string]any{"start_x": vals[0], "start_y": vals[1],
		"end_x": vals[2], "end_y": vals[3], "backend": "wda"}, nil
}

func handleDeviceButton(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.wdaFor(udid)
	if err != nil {
		return nil, err
	}
	name, _ := p["name"].(string)
	if name == "lock" {
		if err := c.Lock(ctx); err != nil {
			return nil, b.wdaFail(udid, "button", err)
		}
		return map[string]any{"button": "lock", "backend": "wda"}, nil
	}
	wdaName, ok := deviceButtons[name]
	if !ok {
		return nil, badRequest("button on a physical device requires payload field %q, one of: %s", "name", deviceButtonNames())
	}
	if err := c.PressButton(ctx, wdaName); err != nil {
		return nil, b.wdaFail(udid, "button", err)
	}
	return map[string]any{"button": name, "backend": "wda"}, nil
}

func handleDevicePasteboardSet(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.wdaFor(udid)
	if err != nil {
		return nil, err
	}
	text, ok := p["text"].(string)
	if !ok {
		return nil, badRequest("pasteboard_set requires a string payload field %q", "text")
	}
	if err := c.SetPasteboard(ctx, text); err != nil {
		return nil, b.wdaFail(udid, "pasteboard_set", err)
	}
	return map[string]any{"copied_runes": len([]rune(text)), "backend": "wda"}, nil
}

func handleDevicePasteboardGet(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	c, err := b.wdaFor(udid)
	if err != nil {
		return nil, err
	}
	text, err := c.GetPasteboard(ctx)
	if err != nil {
		return nil, b.wdaFail(udid, "pasteboard_get", err)
	}
	return map[string]any{"text": text, "backend": "wda"}, nil
}

func handleDeviceTapElement(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	if _, err := b.wdaFor(udid); err != nil {
		return nil, err
	}
	return elemTap(ctx, b, udid, p)
}

func handleDeviceTypeIntoElement(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	if _, err := b.wdaFor(udid); err != nil {
		return nil, err
	}
	return elemTypeInto(ctx, b, udid, p)
}

func handleDeviceWaitForElement(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	if _, err := b.wdaFor(udid); err != nil {
		return nil, err
	}
	return elemWaitFor(ctx, b, udid, p)
}

func handleDeviceWaitTreeStable(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	if _, err := b.wdaFor(udid); err != nil {
		return nil, err
	}
	return elemTreeStable(ctx, b, udid, p)
}

// observeOnce implements elementDriver: one WDA source poll, compacted to
// the shared Node tree. A WDA transport failure mid-lease (the tunnel
// dropped, the runner is restarting) comes back as errNotYet so the wait
// loops keep polling until their own budget runs out. Every WDA source
// read is already an un-cached round trip, so refresh is a no-op here.
func (b *DeviceBackend) observeOnce(ctx context.Context, udid string, _ bool) (observation, error) {
	c, err := b.wdaFor(udid)
	if err != nil {
		return observation{}, err
	}
	src, err := c.Source(ctx)
	if err != nil {
		if isTransientWDAError(err) {
			if k := b.kick[udid]; k != nil {
				k()
			}
			return observation{}, errNotYet
		}
		return observation{}, b.wdaFail(udid, "observe", err)
	}
	nodes, err := CompactWDATree(src)
	if err != nil || len(nodes) == 0 {
		return observation{}, errNotYet
	}
	return observation{nodes: nodes, viewport: wdaViewport(src)}, nil
}

// tapXY implements elementDriver via WDA.
func (b *DeviceBackend) tapXY(ctx context.Context, udid string, x, y float64) error {
	c, err := b.wdaFor(udid)
	if err != nil {
		return err
	}
	if err := c.Tap(ctx, x, y); err != nil {
		return b.wdaFail(udid, "tap", err)
	}
	return nil
}

// validateTypeOpts implements typeOptsValidator: WDA already delivers the
// text app-side in one shot (no per-keystroke hardware-keyboard events),
// so the paste strategy is simulator-only and rejected here.
func (b *DeviceBackend) validateTypeOpts(opts typeOpts) error {
	if opts.strategy == typeStrategyPaste {
		return notImplemented("the %q typing strategy is simulator-only; WDA typing on devices already avoids per-keystroke hardware-keyboard events", typeStrategyPaste)
	}
	return nil
}

// typeInto implements elementDriver via WDA.
func (b *DeviceBackend) typeInto(ctx context.Context, udid, text string, opts typeOpts) (int, error) {
	if err := b.validateTypeOpts(opts); err != nil {
		return 0, err
	}
	c, err := b.wdaFor(udid)
	if err != nil {
		return 0, err
	}
	if err := c.Keys(ctx, text); err != nil {
		return 0, b.wdaFail(udid, "type", err)
	}
	return len([]rune(text)), nil
}

// isTransientWDAError reports whether a WDA failure is likely a dropped
// connection or a restarting agent (worth re-polling) rather than a
// protocol-level rejection.
func isTransientWDAError(err error) bool {
	var we *wda.Error
	if errors.As(err, &we) {
		return we.Status >= 500
	}
	// Non-protocol errors are network-level: connection refused/reset,
	// timeouts — the tunnel or agent going away.
	return true
}
