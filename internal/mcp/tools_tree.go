package mcp

import (
	"context"
)

func toolUITree() Tool {
	return Tool{
		Name:        "ui_tree",
		Description: "Get the current screen as a compact structured accessibility tree: element roles, labels, values, identifiers, frames (x/y/w/h in points), and whether each is interactable — plus a stable tree hash for cheap change detection. Much cheaper than a screenshot and the source of truth for the element matchers used by tap_element / type_into_element / wait_for_element / scroll_to_element. Call it whenever a matcher misses to see what is actually on screen.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string", "description": "Active lease ID from lease_acquire."},
		}, "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			res, err := s.client.Observe(ctx, leaseID)
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
