package mcp

import (
	"context"
	"fmt"
)

func toolState() Tool {
	return Tool{
		Name:        "state",
		Description: "Deterministic simulator state (target derived from lease): action=snapshot returns SnapshotInfo (optional label); restore needs snapshot (ID or label, optional reboot); fixture needs name (e.g. statusbar, privacy, locale) + payload.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string"},
			"action":   {"type": "string", "enum": []string{"snapshot", "restore", "fixture"}},
			"label":    {"type": "string", "description": "for snapshot (optional)"},
			"snapshot": {"type": "string", "description": "for restore: snapshot ID or label"},
			"reboot":   {"type": "boolean", "description": "for restore: shutdown+restore+boot if booted"},
			"name":     {"type": "string", "description": "fixture name"},
			"payload":  {"type": "object", "description": "fixture payload"},
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
