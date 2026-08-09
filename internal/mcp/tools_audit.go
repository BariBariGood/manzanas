package mcp

import (
	"context"

	"github.com/BariBariGood/manzanas/proto"
)

func toolAudit() Tool {
	return Tool{
		Name:        "audit",
		Description: "Run deterministic UI-quality checks over the current screen's accessibility tree and get back FINDINGS — measured evidence, never pass/fail verdicts. Checks: touch_target (interactive elements smaller than 44x44pt), clipping (frames extending past the screen or a non-scrolling parent), alignment (edges almost-but-not-quite aligned, with the delta), spacing (inconsistent gaps in sibling rows/columns), safe_area (interactive elements intruding into safe-area insets), missing_labels (interactive elements a screen reader cannot name). Each finding carries the element's role/label/id/frame, the measured values, and an evidence sentence; its ref (F1, F2, ...) matches a red box drawn on an annotated screenshot. Both the findings JSON and the annotated screenshot are journaled as run artifacts and appear in journal_export, so run audit instead of eyeballing screenshots or hand-measuring ui_tree frames. Dense grids of repeated tiny controls (keyboards, emoji grids, calendar day cells) are suppressed automatically, and so is system chrome (status bar, keyboard, scroll-indicator pseudo-elements) plus small controls inside a full-size tappable list row (Apple's stock Settings rows) — set include_system_chrome / include_covered_controls to audit them anyway. Scope the audit with the matcher fields (only the matched element's subtree is checked) or region. You decide what matters: a finding is a measurement, not a defect verdict.",
		InputSchema: schema(mergeProps(matcherProps(), map[string]map[string]any{
			"checks": {"type": "array", "items": map[string]any{"type": "string",
				"enum": []string{"touch_target", "clipping", "alignment", "spacing", "safe_area", "missing_labels"}},
				"description": "Which checks to run. Omit to run all six."},
			"region": {"type": "object",
				"description": "Audit only elements whose centre lies in this rectangle, in points: {x, y, w, h}. Useful to focus on one screen area without a matcher."},
			"min_touch_pt": {"type": "number", "default": 44,
				"description": "Minimum touch-target size in points for the touch_target check."},
			"alignment_tolerance_pt": {"type": "number", "default": 4,
				"description": "Near-miss window for the alignment check: edge deltas up to this many points are flagged; larger deltas are treated as intentional layout."},
			"spacing_tolerance_pt": {"type": "number", "default": 4,
				"description": "How far a sibling gap may deviate from the group's median before the spacing check flags it, in points."},
			"safe_area_insets": {"type": "object",
				"description": "Explicit safe-area insets in points: {top, bottom, left, right}. Omit to use a device-class heuristic derived from the viewport."},
			"include_system_chrome": {"type": "boolean", "default": false,
				"description": "Also audit OS-drawn chrome (status bar, keyboard, scroll-indicator pseudo-elements), which is suppressed from findings by default."},
			"include_covered_controls": {"type": "boolean", "default": false,
				"description": "Also flag small interactive controls fully covered by an enclosing full-size tappable list row (e.g. the ~28pt buttons inside stock Settings rows), suppressed from touch_target by default because the row provides the touch target."},
		}), "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			payload := elementPayload(args, "checks", "region", "min_touch_pt",
				"alignment_tolerance_pt", "spacing_tolerance_pt", "safe_area_insets",
				"include_system_chrome", "include_covered_controls")
			// The annotated screenshot is journaled server-side; keep the
			// wire response token-cheap for the agent.
			payload["inline"] = false
			res, err := s.client.Dispatch(ctx, proto.ActionRequest{
				LeaseID: leaseID, Kind: "audit", Payload: payload})
			if err != nil {
				return nil, matcherHint("audit", err)
			}
			if err := actionErr(res); err != nil {
				return nil, matcherHint("audit", err)
			}
			return jsonContent(res.Result)
		},
	}
}
