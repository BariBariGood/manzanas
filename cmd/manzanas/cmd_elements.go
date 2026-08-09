package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/BariBariGood/manzanas/proto"
)

// Element matcher commands: resolve an element on the daemon (flat
// matcher flags or the structured --predicate JSON) and act on it in one
// request.

// matcherFlags declares the element-matcher flags shared by the element
// commands and returns a builder that folds them into an action payload.
func matcherFlags(fs *flag.FlagSet) func(payload map[string]any) error {
	label := fs.String("label", "", "match by accessibility label (substring unless --exact)")
	role := fs.String("role", "", "match by role (exact, e.g. Button, Cell)")
	value := fs.String("value", "", "match by value (substring unless --exact)")
	id := fs.String("id", "", "match by accessibility identifier (exact)")
	placeholder := fs.String("placeholder", "", "match by placeholder (substring unless --exact)")
	exact := fs.Bool("exact", false, "text fields must match exactly")
	predicate := fs.String("predicate", "", `structured predicate JSON (e.g. '{"text":"Save","type":"Button"}'; fields: text, text_contains, text_regex, type, accessibility_id, index, bounds_hint, near, parent_of); cannot be combined with the flat matcher flags`)
	timeoutMS := fs.Int("timeout-ms", 0, "polling budget in milliseconds (default 10000)")
	intervalMS := fs.Int("interval-ms", 0, "delay between polls in milliseconds (default 500)")
	return func(payload map[string]any) error {
		for k, v := range map[string]string{"label": *label, "role": *role, "value": *value,
			"id": *id, "placeholder": *placeholder} {
			if v != "" {
				payload[k] = v
			}
		}
		if *exact {
			payload["exact"] = true
		}
		if *predicate != "" {
			var obj map[string]any
			if err := json.Unmarshal([]byte(*predicate), &obj); err != nil {
				return fmt.Errorf("--predicate must be a JSON object: %w", err)
			}
			payload["predicate"] = obj
		}
		if *timeoutMS > 0 {
			payload["timeout_ms"] = *timeoutMS
		}
		if *intervalMS > 0 {
			payload["interval_ms"] = *intervalMS
		}
		return nil
	}
}

// logFlags declares the opt-in log-capture flags shared by the actions
// that support capture_logs and returns a builder that folds them into
// the payload.
func logFlags(fs *flag.FlagSet) func(payload map[string]any) {
	capture := fs.Bool("capture-logs", false, "collect the simulator's os_log lines emitted during the action window (returned as result.logs and journaled)")
	process := fs.String("log-process", "", "only log lines from this process name (implies --capture-logs filtering; use the app's executable name)")
	return func(payload map[string]any) {
		if *capture || *process != "" {
			payload["capture_logs"] = true
		}
		if *process != "" {
			payload["log_process"] = *process
		}
	}
}

// cmdTapElement implements `manzanas tap-element [matcher flags] --lease ID`
// ("tap_element" action): resolve the matcher and tap the match's anchor
// point in one request.
func cmdTapElement(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("tap-element")
	lease := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID (or $MANZANAS_LEASE)")
	anchor := fs.String("anchor", "", "where on the frame to tap: center (default), start, end")
	buildMatcher := matcherFlags(fs)
	buildLogs := logFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("tap-element: unexpected argument %q", fs.Arg(0))
	}
	if *lease == "" {
		return fmt.Errorf("tap-element: --lease (or $MANZANAS_LEASE) is required")
	}
	payload := map[string]any{}
	if err := buildMatcher(payload); err != nil {
		return fmt.Errorf("tap-element: %w", err)
	}
	if *anchor != "" {
		payload["anchor"] = *anchor
	}
	buildLogs(payload)
	res, err := app.client.Dispatch(ctx, proto.ActionRequest{
		LeaseID: *lease, Kind: "tap_element", Payload: payload})
	if err != nil {
		return err
	}
	return emitActionResult(app, res)
}

// cmdWaitForElement implements `manzanas wait-for-element [matcher flags]
// [--absent] --lease ID` ("wait_for_element" action).
func cmdWaitForElement(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("wait-for-element")
	lease := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID (or $MANZANAS_LEASE)")
	absent := fs.Bool("absent", false, "wait for the element to disappear instead")
	buildMatcher := matcherFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("wait-for-element: unexpected argument %q", fs.Arg(0))
	}
	if *lease == "" {
		return fmt.Errorf("wait-for-element: --lease (or $MANZANAS_LEASE) is required")
	}
	payload := map[string]any{}
	if err := buildMatcher(payload); err != nil {
		return fmt.Errorf("wait-for-element: %w", err)
	}
	if *absent {
		payload["absent"] = true
	}
	res, err := app.client.Dispatch(ctx, proto.ActionRequest{
		LeaseID: *lease, Kind: "wait_for_element", Payload: payload})
	if err != nil {
		return err
	}
	return emitActionResult(app, res)
}

// cmdTypeIntoElement implements `manzanas type-into-element TEXT [matcher
// flags] --lease ID` ("type_into_element" action).
func cmdTypeIntoElement(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("type-into-element")
	lease := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID (or $MANZANAS_LEASE)")
	anchor := fs.String("anchor", "", "where on the frame to tap when focusing: center (default), start, end")
	strategy := fs.String("strategy", "", "typing strategy: hid (default) or paste")
	buildMatcher := matcherFlags(fs)
	buildLogs := logFlags(fs)
	literal := len(args) > 0 && args[0] == "--"
	if literal {
		args = args[1:]
	}
	if len(args) < 1 {
		return fmt.Errorf("type-into-element: expected TEXT argument")
	}
	if !literal && strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("type-into-element: positional arguments must come before flags (got %q; use %q before them to pass literal dash-prefixed values)", args[0], "--")
	}
	text := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("type-into-element: unexpected argument %q", fs.Arg(0))
	}
	if *lease == "" {
		return fmt.Errorf("type-into-element: --lease (or $MANZANAS_LEASE) is required")
	}
	payload := map[string]any{"text": text}
	if err := buildMatcher(payload); err != nil {
		return fmt.Errorf("type-into-element: %w", err)
	}
	if *anchor != "" {
		payload["anchor"] = *anchor
	}
	if *strategy != "" {
		payload["strategy"] = *strategy
	}
	buildLogs(payload)
	res, err := app.client.Dispatch(ctx, proto.ActionRequest{
		LeaseID: *lease, Kind: "type_into_element", Payload: payload})
	if err != nil {
		return err
	}
	return emitActionResult(app, res)
}
