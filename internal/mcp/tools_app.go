package mcp

import (
	"context"
	"fmt"
)

func toolApp() Tool {
	return Tool{
		Name:        "app",
		Description: "App lifecycle on the leased target. action=install needs path (a built .app bundle on the daemon's host filesystem, not this machine); launch/terminate need bundle_id.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id":  {"type": "string", "description": "active lease, from lease_acquire"},
			"action":    {"type": "string", "enum": []string{"install", "launch", "terminate"}, "description": "which lifecycle operation to run"},
			"path":      {"type": "string", "description": "install only: absolute path to a .app bundle on the daemon host"},
			"bundle_id": {"type": "string", "description": "launch/terminate only: the app's bundle identifier, e.g. com.example.myapp"},
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
