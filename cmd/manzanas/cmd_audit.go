package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/BariBariGood/manzanas/internal/client"
	"github.com/BariBariGood/manzanas/proto"
)

// cmdAudit implements `manzanas audit --lease ID` ("audit" action):
// deterministic UI-quality checks over the accessibility tree, returning
// findings (evidence, not verdicts) and an annotated screenshot with red
// boxes around each finding's element. Both are journaled as run
// artifacts; -o additionally writes the annotated PNG locally.
func cmdAudit(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("audit")
	lease := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID (or $MANZANAS_LEASE)")
	out := fs.String("o", "", "write the annotated screenshot to FILE.png")
	checks := fs.String("checks", "", "comma-separated checks to run (default all: touch_target,clipping,alignment,spacing,safe_area,missing_labels)")
	region := fs.String("region", "", "audit only elements centred in X,Y,W,H (points)")
	minTouch := fs.Float64("min-touch", 0, "minimum touch target in points (default 44)")
	alignTol := fs.Float64("align-tol", 0, "near-miss alignment tolerance in points (default 4)")
	spacingTol := fs.Float64("spacing-tol", 0, "sibling spacing tolerance in points (default 4)")
	includeChrome := fs.Bool("include-system-chrome", false, "also audit system chrome (scroll indicators, status bar, keyboard container) that is suppressed by default; individual keyboard keys stay under the dense-group rule")
	includeCovered := fs.Bool("include-covered-controls", false, "also flag small controls whose touch target is covered by an enclosing tappable list row")
	buildMatcher := matcherFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("audit: unexpected argument %q", fs.Arg(0))
	}
	if *lease == "" {
		return fmt.Errorf("audit: --lease (or $MANZANAS_LEASE) is required")
	}
	payload := map[string]any{}
	if err := buildMatcher(payload); err != nil {
		return err
	}
	if *checks != "" {
		list := []any{}
		for _, c := range strings.Split(*checks, ",") {
			if c = strings.TrimSpace(c); c != "" {
				list = append(list, c)
			}
		}
		payload["checks"] = list
	}
	if *region != "" {
		parts := strings.Split(*region, ",")
		if len(parts) != 4 {
			return fmt.Errorf("audit: --region must be X,Y,W,H")
		}
		keys := []string{"x", "y", "w", "h"}
		r := map[string]any{}
		for i, p := range parts {
			v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
			if err != nil {
				return fmt.Errorf("audit: --region value %q is not a number", p)
			}
			r[keys[i]] = v
		}
		payload["region"] = r
	}
	for k, v := range map[string]float64{"min_touch_pt": *minTouch,
		"alignment_tolerance_pt": *alignTol, "spacing_tolerance_pt": *spacingTol} {
		if v > 0 {
			payload[k] = v
		}
	}
	if *includeChrome {
		payload["include_system_chrome"] = true
	}
	if *includeCovered {
		payload["include_covered_controls"] = true
	}
	if *out == "" {
		// Findings on stdout; the annotated screenshot lives on as a
		// journal artifact without inflating the response.
		payload["inline"] = false
	}
	res, err := app.client.Dispatch(ctx, proto.ActionRequest{
		LeaseID: *lease, Kind: "audit", Payload: payload})
	if err != nil {
		return err
	}
	if *out != "" && res.OK {
		// The daemon degrades a failed capture to a screenshot_error while
		// keeping the findings; emit them instead of failing the command.
		if b64 := client.ImageB64(res); b64 == "" {
			fmt.Fprintf(app.stderr, "audit: no annotated image data (%v); findings follow\n",
				res.Result["screenshot_error"])
		} else {
			data, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return fmt.Errorf("audit: invalid base64 image data: %w", err)
			}
			if err := os.WriteFile(*out, data, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(app.stderr, "wrote %s (%d bytes)\n", *out, len(data))
			delete(res.Result, "png_base64")
		}
	}
	return emitActionResult(app, res)
}
