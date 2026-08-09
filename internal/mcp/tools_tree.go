package mcp

import (
	"context"

	"github.com/BariBariGood/manzanas/proto"
)

func toolUITree() Tool {
	return Tool{
		Name:        "ui_tree",
		Description: "Get the current screen as a compact structured accessibility tree: element roles, labels, values, identifiers, frames (x/y/w/h in points), and whether each is interactable — plus a stable tree hash for cheap change detection. Much cheaper than a screenshot and the source of truth for the element matchers used by tap_element / type_into_element / wait_for_element / scroll_to_element. Call it whenever a matcher misses to see what is actually on screen. To cut tokens on busy screens, use format:\"compact\" (one line per element with stable [i] indexes), interactive_only, roles, scope (one subtree), or exclude_system_chrome — the filters compose and the hash always reflects the full tree.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string", "description": "Active lease ID from lease_acquire."},
			"format": {"type": "string", "enum": []string{"json", "compact"},
				"description": "compact returns tree_compact: one line per element, depth-indented, prefixed with a stable depth-first [i] index — far fewer tokens than the nested JSON tree. Default json."},
			"interactive_only": {"type": "boolean", "default": false,
				"description": "Only return interactable elements (buttons, fields, cells...); non-interactive ancestors are kept so the structure stays connected."},
			"roles": {"type": "array", "items": map[string]any{"type": "string"},
				"description": "Only return elements with these roles (e.g. [\"Button\",\"Cell\"]); ancestors are kept for structure."},
			"scope": {"type": "object",
				"description": "Narrow to one element's subtree: a structured predicate object (same fields as the predicate matcher, e.g. {\"type\":\"Table\"}). Must match exactly one element."},
			"exclude_system_chrome": {"type": "boolean", "default": false,
				"description": "Drop OS-drawn chrome — status bar, keyboard, scroll-indicator pseudo-elements — leaving only app content."},
		}, "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			payload := map[string]any{}
			for _, k := range []string{"format", "interactive_only", "roles",
				"scope", "exclude_system_chrome"} {
				if v, ok := args[k]; ok {
					payload[k] = v
				}
			}
			res, err := s.client.Dispatch(ctx, proto.ActionRequest{
				LeaseID: leaseID, Kind: "observe", Payload: payload})
			if err != nil {
				return nil, err
			}
			if err := actionErr(res); err != nil {
				return nil, err
			}
			return jsonContent(res.Result)
		},
	}
}
