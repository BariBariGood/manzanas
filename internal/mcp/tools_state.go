package mcp

import (
	"context"
	"fmt"
)

func toolState() Tool {
	return Tool{
		Name:        "state",
		Description: "Deterministic simulator state control (target derived from the lease): action=snapshot saves the current state; restore returns to a saved snapshot; fixture applies a named environment preset (clean status bar, granted privacy permissions, locale, timezone, push payload, open URL).",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string", "description": "active lease, from lease_acquire"},
			"action":   {"type": "string", "enum": []string{"snapshot", "restore", "fixture"}, "description": "which state operation to run"},
			"label":    {"type": "string", "description": "snapshot only: optional human-readable label to restore by later"},
			"snapshot": {"type": "string", "description": "restore only: snapshot ID or label from a previous snapshot"},
			"reboot":   {"type": "boolean", "description": "restore only: shutdown + restore + boot when the target is booted"},
			"name":     {"type": "string", "enum": []string{"statusbar", "privacy", "push", "locale", "timezone", "url"}, "description": "fixture only: which fixture to apply"},
			"payload":  {"type": "object", "description": "fixture only: fixture-specific parameters (see docs/state.md), e.g. {\"time\": \"9:41\"} for statusbar"},
		}, "lease_id", "action"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			switch str(args, "action") {
			case "snapshot":
				info, err := s.client.StateSnapshot(ctx, leaseID, str(args, "label"))
				if err != nil {
					return nil, err
				}
				return jsonContent(info)
			case "restore":
				snapshot, err := reqStr(args, "snapshot")
				if err != nil {
					return nil, err
				}
				reboot, _ := args["reboot"].(bool)
				res, err := s.client.StateRestore(ctx, leaseID, snapshot, reboot)
				if err != nil {
					return nil, err
				}
				return jsonContent(res)
			case "fixture":
				name, err := reqStr(args, "name")
				if err != nil {
					return nil, err
				}
				payload, _ := args["payload"].(map[string]any)
				if err := s.client.StateFixture(ctx, leaseID, name, payload); err != nil {
					return nil, err
				}
				return textContent("ok"), nil
			default:
				return nil, fmt.Errorf("action must be snapshot, restore, or fixture")
			}
		},
	}
}
