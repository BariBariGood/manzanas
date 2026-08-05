package mcp

import (
	"context"
	"fmt"
)

func toolApp() Tool {
	return Tool{
		Name:        "app",
		Description: "App lifecycle on the leased simulator. action=install needs path (a .app on the daemon host); launch/terminate need bundle_id.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id":  {"type": "string"},
			"action":    {"type": "string", "enum": []string{"install", "launch", "terminate"}},
			"path":      {"type": "string", "description": "for install"},
			"bundle_id": {"type": "string", "description": "for launch/terminate"},
		}, "lease_id", "action"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			switch str(args, "action") {
			case "install":
				path, err := reqStr(args, "path")
				if err != nil {
					return nil, err
				}
				res, err := s.client.AppInstall(ctx, leaseID, path)
				if err != nil {
					return nil, err
				}
				return actionContent(res)
			case "launch":
				bundleID, err := reqStr(args, "bundle_id")
				if err != nil {
					return nil, err
				}
				res, err := s.client.AppLaunch(ctx, leaseID, bundleID)
				if err != nil {
					return nil, err
				}
				return actionContent(res)
			case "terminate":
				bundleID, err := reqStr(args, "bundle_id")
				if err != nil {
					return nil, err
				}
				res, err := s.client.AppTerminate(ctx, leaseID, bundleID)
				if err != nil {
					return nil, err
				}
				return actionContent(res)
			default:
				return nil, fmt.Errorf("action must be install, launch, or terminate")
			}
		},
	}
}
