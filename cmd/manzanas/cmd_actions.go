package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/BariBariGood/manzanas/internal/client"

	"github.com/BariBariGood/manzanas/proto"
)

// leaseFlag parses the --lease flag shared by every action command.
// Positional args come first; flags after them. A leading "--" marks
// everything up to the flag section as literal (allows dash-prefixed text).
func leaseFlag(app *appEnv, name string, args []string, positional int) (leaseID string, pos []string, err error) {
	fs := app.newFlagSet(name)
	lease := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID (or $MANZANAS_LEASE)")
	literal := len(args) > 0 && args[0] == "--"
	if literal {
		args = args[1:]
	}
	if len(args) < positional {
		return "", nil, fmt.Errorf("%s: expected %d argument(s)", name, positional)
	}
	if !literal {
		for _, a := range args[:positional] {
			if strings.HasPrefix(a, "-") {
				return "", nil, fmt.Errorf("%s: positional arguments must come before flags (got %q; use %q before them to pass literal dash-prefixed values)", name, a, "--")
			}
		}
	}
	if err := fs.Parse(args[positional:]); err != nil {
		return "", nil, err
	}
	if fs.NArg() > 0 {
		return "", nil, fmt.Errorf("%s: unexpected argument %q (expected %d positional argument(s) before flags)", name, fs.Arg(0), positional)
	}
	if *lease == "" {
		return "", nil, fmt.Errorf("%s: --lease (or $MANZANAS_LEASE) is required", name)
	}
	return *lease, args[:positional], nil
}

func emitActionResult(app *appEnv, res proto.ActionResult) error {
	if err := app.emit(res, func(w io.Writer) {
		if res.OK {
			fmt.Fprintln(w, "ok")
		} else {
			fmt.Fprintln(w, "failed")
		}
		if len(res.Result) > 0 {
			b, _ := json.MarshalIndent(res.Result, "", "  ")
			fmt.Fprintln(w, string(b))
		}
	}); err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("action failed")
	}
	return nil
}

// cmdTap implements `manzanas tap X Y --lease ID` ("tap" action).
func cmdTap(ctx context.Context, app *appEnv, args []string) error {
	leaseID, pos, err := leaseFlag(app, "tap", args, 2)
	if err != nil {
		return err
	}
	x, err1 := strconv.ParseFloat(pos[0], 64)
	y, err2 := strconv.ParseFloat(pos[1], 64)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("tap: X and Y must be numbers")
	}
	res, err := app.client.Tap(ctx, leaseID, x, y)
	if err != nil {
		return err
	}
	return emitActionResult(app, res)
}

// cmdSwipe implements `manzanas swipe X1 Y1 X2 Y2 [--duration-ms N] --lease ID`.
func cmdSwipe(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("swipe")
	lease := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID")
	dur := fs.Int("duration-ms", 0, "swipe duration in milliseconds")
	if len(args) < 4 {
		return fmt.Errorf("swipe: expected X1 Y1 X2 Y2")
	}
	if err := fs.Parse(args[4:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("swipe: unexpected argument %q (expected X1 Y1 X2 Y2 before flags)", fs.Arg(0))
	}
	if *lease == "" {
		return fmt.Errorf("swipe: --lease (or $MANZANAS_LEASE) is required")
	}
	coords := make([]float64, 4)
	for i := 0; i < 4; i++ {
		v, err := strconv.ParseFloat(args[i], 64)
		if err != nil {
			return fmt.Errorf("swipe: coordinates must be numbers")
		}
		coords[i] = v
	}
	res, err := app.client.Swipe(ctx, *lease, coords[0], coords[1], coords[2], coords[3], *dur)
	if err != nil {
		return err
	}
	return emitActionResult(app, res)
}

// cmdType implements `manzanas type TEXT --lease ID` ("type" action).
func cmdType(ctx context.Context, app *appEnv, args []string) error {
	leaseID, pos, err := leaseFlag(app, "type", args, 1)
	if err != nil {
		return err
	}
	res, err := app.client.Type(ctx, leaseID, pos[0])
	if err != nil {
		return err
	}
	return emitActionResult(app, res)
}

// cmdButton implements `manzanas button NAME --lease ID` ("button" action;
// names like home, lock, side-button, siri, apple-pay).
func cmdButton(ctx context.Context, app *appEnv, args []string) error {
	leaseID, pos, err := leaseFlag(app, "button", args, 1)
	if err != nil {
		return err
	}
	res, err := app.client.Button(ctx, leaseID, pos[0])
	if err != nil {
		return err
	}
	return emitActionResult(app, res)
}

// cmdObserve implements `manzanas observe --lease ID` ("observe" action,
// compact a11y tree).
func cmdObserve(ctx context.Context, app *appEnv, args []string) error {
	leaseID, _, err := leaseFlag(app, "observe", args, 0)
	if err != nil {
		return err
	}
	res, err := app.client.Observe(ctx, leaseID)
	if err != nil {
		return err
	}
	return emitActionResult(app, res)
}

// cmdScreenshot implements `manzanas screenshot -o FILE --lease ID`
// ("screenshot" action; the daemon returns base64 image data under a
// format-derived key inside `result`, e.g. `result.png_base64` /
// `result.jpeg_base64`). `-o -` writes the raw image bytes to stdout.
// Status text always goes to stderr so stdout stays clean for piping.
func cmdScreenshot(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("screenshot")
	lease := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID")
	out := fs.String("o", "", "output file (required, e.g. shot.png; - writes raw image bytes to stdout)")
	format := fs.String("format", "", "image format: png (default) or jpeg")
	quality := fs.Int("quality", 0, "JPEG quality 1-100 (jpeg only)")
	maxDim := fs.Int("max-dim", 0, "longer-edge pixel cap (server-side downscale)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("screenshot: unexpected argument %q", fs.Arg(0))
	}
	if *lease == "" {
		return fmt.Errorf("screenshot: --lease (or $MANZANAS_LEASE) is required")
	}
	if *out == "" {
		return fmt.Errorf("screenshot: -o FILE.png is required (use -o - for raw bytes on stdout)")
	}
	if *out == "-" && app.json {
		return fmt.Errorf("screenshot: --json cannot be combined with -o - (stdout carries the raw image bytes)")
	}
	res, err := app.client.ScreenshotOpts(ctx, *lease, *format, *quality, *maxDim)
	if err != nil {
		return err
	}
	if !res.OK {
		if len(res.Result) > 0 {
			b, _ := json.Marshal(res.Result)
			return fmt.Errorf("screenshot: action failed: %s", b)
		}
		return fmt.Errorf("screenshot: action failed")
	}
	b64 := client.ImageB64(res)
	if b64 == "" {
		return fmt.Errorf("screenshot: daemon returned no image data")
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("screenshot: invalid base64 image data: %w", err)
	}
	if *out == "-" {
		if _, err := app.stdout.Write(data); err != nil {
			return err
		}
		fmt.Fprintf(app.stderr, "wrote %d bytes to stdout\n", len(data))
		return nil
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return err
	}
	if app.json {
		return app.printJSON(map[string]any{"file": *out, "bytes": len(data)})
	}
	fmt.Fprintf(app.stderr, "wrote %s (%d bytes)\n", *out, len(data))
	return nil
}

// cmdScrollToElement implements `manzanas scroll-to-element --label TEXT
// [--direction down] --lease ID` ("scroll_to_element" action): scroll a
// container until the matched element is inside the viewport.
func cmdScrollToElement(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("scroll-to-element")
	lease := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID (or $MANZANAS_LEASE)")
	label := fs.String("label", "", "match by accessibility label (substring unless --exact)")
	role := fs.String("role", "", "match by role (exact, e.g. Button, Cell)")
	value := fs.String("value", "", "match by value (substring unless --exact)")
	id := fs.String("id", "", "match by accessibility identifier (exact)")
	placeholder := fs.String("placeholder", "", "match by placeholder (substring unless --exact)")
	exact := fs.Bool("exact", false, "text fields must match exactly")
	direction := fs.String("direction", "", "scroll direction when the element is not visible yet: down (default), up, left, right")
	maxScrolls := fs.Int("max-scrolls", 0, "max swipe attempts (default 8, max 30)")
	timeoutMS := fs.Int("timeout-ms", 0, "overall budget in milliseconds (default 30000)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("scroll-to-element: unexpected argument %q", fs.Arg(0))
	}
	if *lease == "" {
		return fmt.Errorf("scroll-to-element: --lease (or $MANZANAS_LEASE) is required")
	}
	payload := map[string]any{}
	for k, v := range map[string]string{"label": *label, "role": *role, "value": *value,
		"id": *id, "placeholder": *placeholder, "direction": *direction} {
		if v != "" {
			payload[k] = v
		}
	}
	if *exact {
		payload["exact"] = true
	}
	if *maxScrolls > 0 {
		payload["max_scrolls"] = *maxScrolls
	}
	if *timeoutMS > 0 {
		payload["timeout_ms"] = *timeoutMS
	}
	res, err := app.client.Dispatch(ctx, proto.ActionRequest{
		LeaseID: *lease, Kind: "scroll_to_element", Payload: payload})
	if err != nil {
		return err
	}
	return emitActionResult(app, res)
}

// cmdApp implements `manzanas app install|launch|terminate` (app.* actions).
func cmdApp(ctx context.Context, app *appEnv, args []string) error {
	sub, err := requireArg(args, 0, "app subcommand (install|launch|terminate)")
	if err != nil {
		return err
	}
	leaseID, pos, err := leaseFlag(app, "app "+sub, args[1:], 1)
	if err != nil {
		return err
	}
	var res proto.ActionResult
	switch sub {
	case "install":
		res, err = app.client.AppInstall(ctx, leaseID, pos[0])
	case "launch":
		res, err = app.client.AppLaunch(ctx, leaseID, pos[0])
	case "terminate":
		res, err = app.client.AppTerminate(ctx, leaseID, pos[0])
	default:
		return fmt.Errorf("unknown app subcommand %q", sub)
	}
	if err != nil {
		return err
	}
	return emitActionResult(app, res)
}
