package actions

import (
	"context"
	"strconv"
	"strings"
)

func handleInstallApp(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	path, _ := p["path"].(string)
	if path == "" {
		return nil, badRequest("install_app requires payload field %q (path to a .app bundle on the daemon host)", "path")
	}
	if _, err := b.simctl(ctx, "install", udid, path); err != nil {
		return nil, err
	}
	return map[string]any{"installed": path}, nil
}

func handleLaunchApp(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	bundleID, _ := p["bundle_id"].(string)
	if bundleID == "" {
		return nil, badRequest("launch_app requires payload field %q", "bundle_id")
	}
	args := []string{"launch"}
	terminate, err := boolFlag(p, "terminate_running", false)
	if err != nil {
		return nil, err
	}
	if terminate {
		args = append(args, "--terminate-running-process")
	}
	args = append(args, udid, bundleID)
	if extra, ok := p["args"].([]any); ok {
		for _, a := range extra {
			s, ok := a.(string)
			if !ok {
				return nil, badRequest("launch_app args must be strings")
			}
			args = append(args, s)
		}
	}
	out, err := b.simctl(ctx, args...)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"bundle_id": bundleID}
	// simctl prints "<bundle id>: <pid>".
	if _, pid, ok := strings.Cut(strings.TrimSpace(string(out)), ": "); ok {
		if n, err := strconv.Atoi(pid); err == nil {
			res["pid"] = n
		} else {
			res["pid"] = pid
		}
	}
	return res, nil
}

func handleTerminateApp(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	bundleID, _ := p["bundle_id"].(string)
	if bundleID == "" {
		return nil, badRequest("terminate_app requires payload field %q", "bundle_id")
	}
	if _, err := b.simctl(ctx, "terminate", udid, bundleID); err != nil {
		return nil, err
	}
	return map[string]any{"terminated": bundleID}, nil
}
