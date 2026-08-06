package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// validButtons are the hardware buttons AXe can press. AXe has no
// volume-up/volume-down support, so they are rejected up front rather
// than surfacing a CLI failure.
var validButtons = map[string]bool{
	"home": true, "lock": true, "side-button": true, "siri": true,
	"apple-pay": true,
}

func handleTap(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	x, err := coordField(p, "x")
	if err != nil {
		return nil, err
	}
	y, err := coordField(p, "y")
	if err != nil {
		return nil, err
	}
	if _, err := b.axe(ctx, udid, "tap", "-x", fmtNum(x), "-y", fmtNum(y)); err != nil {
		return nil, err
	}
	return map[string]any{"x": x, "y": y}, nil
}

func handleSwipe(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	var vals [4]float64
	for i, k := range []string{"start_x", "start_y", "end_x", "end_y"} {
		v, err := coordField(p, k)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	args := []string{"swipe",
		"--start-x", fmtNum(vals[0]), "--start-y", fmtNum(vals[1]),
		"--end-x", fmtNum(vals[2]), "--end-y", fmtNum(vals[3]),
	}
	if d, ok := p["duration_seconds"]; ok {
		dv, err := toNum(d)
		if err != nil || dv <= 0 {
			return nil, badRequest("duration_seconds must be a positive number")
		}
		args = append(args, "--duration", fmtNum(dv))
	}
	if _, err := b.axe(ctx, udid, args...); err != nil {
		return nil, err
	}
	return map[string]any{"start_x": vals[0], "start_y": vals[1], "end_x": vals[2], "end_y": vals[3]}, nil
}

func handleType(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	text, ok := p["text"].(string)
	if !ok || text == "" {
		return nil, badRequest("type requires a non-empty string payload field %q", "text")
	}
	opts, err := typeOptsFromPayload(p)
	if err != nil {
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
	typed, err := b.typeInto(ctx, udid, text, opts)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"typed_runes": typed}
	if opts.strategy == typeStrategyPaste {
		res["strategy"] = typeStrategyPaste
	}
	return res, nil
}

// Typing strategies for type / type_into_element.
const (
	// typeStrategyHID synthesizes one hardware-keyboard event per rune
	// (the historical behavior).
	typeStrategyHID = "hid"
	// typeStrategyPaste copies the text onto the simulator pasteboard
	// (`simctl pbcopy`) and sends a single Cmd-V chord, avoiding
	// per-keystroke hardware-keyboard events entirely. On iOS 26.5 sims
	// slimmed without the speech daemons (assistantd/corespeechd),
	// per-keystroke typing wedges the frontmost app's main thread in
	// AFDictationConnection; paste side-steps that path. Tradeoff: the
	// simulator pasteboard is overwritten, the field receives the text
	// in one shot (per-keystroke handlers fire once), and the field must
	// accept paste.
	typeStrategyPaste = "paste"
)

// typeOpts carries the optional typing payload fields shared by type and
// type_into_element.
type typeOpts struct {
	strategy     string
	requireFocus bool
}

func typeOptsFromPayload(p map[string]any) (typeOpts, error) {
	opts := typeOpts{strategy: typeStrategyHID}
	if v, ok := p["strategy"]; ok {
		s, ok := v.(string)
		if !ok || (s != typeStrategyHID && s != typeStrategyPaste) {
			return opts, badRequest("payload field %q must be %q or %q", "strategy", typeStrategyHID, typeStrategyPaste)
		}
		opts.strategy = s
	}
	rf, err := boolFlag(p, "require_focus", false)
	if err != nil {
		return opts, err
	}
	opts.requireFocus = rf
	return opts, nil
}

// typeInto types text into the focused field, returning the rune count.
// Dispatch prefers the resident warm helper (falling back to the AXe CLI
// when the helper is absent or doesn't implement "type").
func (b *AXeBackend) typeInto(ctx context.Context, udid, text string, opts typeOpts) (int, error) {
	if opts.strategy == typeStrategyPaste {
		return b.pasteInto(ctx, udid, text)
	}
	if err := b.dispatchHID(ctx, udid, "type", map[string]any{"text": text},
		"type", text); err != nil {
		// Only characters with a HID keycode can be synthesized; a
		// character outside that set is a caller error, not a host fault.
		// Any prefix before the offending rune may already have been typed.
		if e, ok := err.(*Error); ok && strings.Contains(strings.ToLower(e.Message), "no keycode found") {
			return 0, badRequest("text contains a character that cannot be typed (no HID keycode); earlier characters may have been typed: %s", e.Message)
		}
		return 0, err
	}
	return len([]rune(text)), nil
}

// HID usage codes for the paste chord (USB HID keyboard usage page).
const (
	hidKeyLeftGUI = 227 // Left Command
	hidKeyV       = 25
)

// pasteInto implements the paste typing strategy: pasteboard set via
// `simctl pbcopy`, then one Cmd-V chord. The chord prefers the resident
// warm helper's key_combo op; a helper predating it falls back to
// `axe key-combo`.
func (b *AXeBackend) pasteInto(ctx context.Context, udid, text string) (int, error) {
	if _, err := b.simctlInput(ctx, []byte(text), "pbcopy", udid); err != nil {
		return 0, err
	}
	err := b.dispatchHID(ctx, udid, "key_combo",
		map[string]any{"modifiers": []any{float64(hidKeyLeftGUI)}, "keycode": float64(hidKeyV)},
		"key-combo", "--modifiers", strconv.Itoa(hidKeyLeftGUI), "--key", strconv.Itoa(hidKeyV))
	if err != nil {
		if e, ok := err.(*Error); ok && axePredatesKeyCombo(e.Message) {
			return 0, unavailable("the installed AXe predates `key-combo` (needed for the paste typing strategy); upgrade AXe or deploy the simbridge warm helper: %s", e.Message)
		}
		return 0, err
	}
	return len([]rune(text)), nil
}

// axePredatesKeyCombo recognizes an AXe CLI rejecting the key-combo
// subcommand (added after v1.8), so the failure surfaces as an
// actionable "upgrade AXe" instead of a raw usage error.
func axePredatesKeyCombo(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"unexpected argument", "unknown command", "unknown subcommand", "unrecognized", "help for axe"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// tapXY taps at screen coordinates, preferring the resident warm helper.
func (b *AXeBackend) tapXY(ctx context.Context, udid string, x, y float64) error {
	return b.dispatchHID(ctx, udid, "tap", map[string]any{"x": x, "y": y},
		"tap", "-x", fmtNum(x), "-y", fmtNum(y))
}

func handleButton(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	name, _ := p["name"].(string)
	if !validButtons[name] {
		return nil, badRequest("button requires payload field %q, one of: %s", "name", buttonNames())
	}
	if _, err := b.axe(ctx, udid, "button", name); err != nil {
		return nil, err
	}
	return map[string]any{"button": name}, nil
}

func handleKey(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	code, err := numField(p, "keycode")
	if err != nil {
		return nil, err
	}
	if !keycodeInRange(code) {
		return nil, badRequest("keycode out of range")
	}
	args := []string{"key", strconv.Itoa(int(code))}
	if d, ok := p["duration_seconds"]; ok {
		dv, err := toNum(d)
		if err != nil || dv <= 0 {
			return nil, badRequest("duration_seconds must be a positive number")
		}
		args = append(args, "--duration", fmtNum(dv))
	}
	if _, err := b.axe(ctx, udid, args...); err != nil {
		return nil, err
	}
	return map[string]any{"keycode": int(code)}, nil
}

func handleKeySequence(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	raw, ok := p["keycodes"].([]any)
	if !ok || len(raw) == 0 {
		return nil, badRequest("key_sequence requires a non-empty array payload field %q", "keycodes")
	}
	codes := make([]string, 0, len(raw))
	for i, r := range raw {
		v, err := toNum(r)
		if err != nil {
			return nil, badRequest("keycodes[%d] must be a number", i)
		}
		if !keycodeInRange(v) {
			return nil, badRequest("keycodes[%d] out of range", i)
		}
		codes = append(codes, strconv.Itoa(int(v)))
	}
	if _, err := b.axe(ctx, udid, "key-sequence", "--keycodes", strings.Join(codes, ",")); err != nil {
		return nil, err
	}
	return map[string]any{"count": len(codes)}, nil
}

func buttonNames() string {
	names := make([]string, 0, len(validButtons))
	for n := range validButtons {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// coordField extracts a required screen coordinate; negative values are
// rejected here because AXe's flag parser would misread "-x -5" as a
// missing flag value.
func coordField(p map[string]any, key string) (float64, error) {
	v, err := numField(p, key)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, badRequest("payload field %q must be >= 0, got %s", key, fmtNum(v))
	}
	return v, nil
}

// numField extracts a required numeric payload field.
func numField(p map[string]any, key string) (float64, error) {
	v, ok := p[key]
	if !ok {
		return 0, badRequest("missing required numeric payload field %q", key)
	}
	n, err := toNum(v)
	if err != nil {
		return 0, badRequest("payload field %q must be a number", key)
	}
	return n, nil
}

func toNum(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case json.Number:
		return n.Float64()
	}
	return 0, fmt.Errorf("not a number: %T", v)
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
