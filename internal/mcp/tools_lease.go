package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

func toolLeaseAcquire() Tool {
	return Tool{
		Name:        "lease_acquire",
		Description: "Claim an iOS simulator or physical device matching labels (e.g. [\"ios26\",\"iphone-17-pro\"]; use [\"device\"] for a physical device). Returns lease_id + target_udid. All other tools need the lease_id. Waits if targets are busy.",
		InputSchema: schema(map[string]map[string]any{
			"labels":      {"type": "array", "items": map[string]any{"type": "string"}, "description": "target labels; empty = any"},
			"udid":        {"type": "string", "description": "pin the lease to this target UDID instead of label matching"},
			"ttl_seconds": {"type": "integer", "description": "lease TTL, default 300, max 3600"},
			"wait":        {"type": "boolean", "description": "wait if queued (default true)"},
			"record":      {"type": "string", "enum": []string{"video"}, "description": "auto-record the whole lease as video; the mp4 lands in the run journal at lease end"},
		}),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			var labels []string
			if raw, ok := args["labels"].([]any); ok {
				for _, l := range raw {
					if ls, ok := l.(string); ok {
						labels = append(labels, ls)
					}
				}
			}
			l, err := s.client.AcquireLease(ctx, proto.AcquireLeaseRequest{
				Labels: labels, UDID: str(args, "udid"), AgentID: "manzanas-mcp",
				TTLSeconds: int(num(args, "ttl_seconds")),
				Record:     str(args, "record"),
			})
			if err != nil {
				return nil, err
			}
			wait := true
			if w, ok := args["wait"].(bool); ok {
				wait = w
			}
			s.trackLease(l.ID)
			if l.State == proto.LeaseQueued && wait {
				l, err = s.client.WaitForLease(ctx, l.ID, 2*time.Second, nil)
				if err != nil {
					return nil, err
				}
			}
			return jsonContent(l)
		},
	}
}

func toolLeaseRelease() Tool {
	return Tool{
		Name:        "lease_release",
		Description: "Release a lease when done so other agents can use the simulator.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string"},
		}, "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			id := str(args, "lease_id")
			if id == "" {
				return nil, errors.New("lease_id is required")
			}
			l, err := s.client.ReleaseLease(ctx, id)
			if err != nil {
				return nil, err
			}
			s.forgetLease(id)
			return jsonContent(l)
		},
	}
}

func toolTargets() Tool {
	return Tool{
		Name:        "targets",
		Description: "List simulators and physical devices in the fleet: udid, kind, name, runtime, state, labels. Physical devices have kind \"device\" (plus a \"disconnected\" label when their tunnel is down).",
		InputSchema: schema(map[string]map[string]any{
			"kind": {"type": "string", "enum": []string{"simulator", "device"}, "description": "only targets of this kind"},
		}),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			targets, err := s.client.ListTargets(ctx)
			if err != nil {
				return nil, err
			}
			if kind := str(args, "kind"); kind != "" {
				filtered := targets[:0]
				for _, t := range targets {
					if string(t.Kind) == kind {
						filtered = append(filtered, t)
					}
				}
				targets = filtered
			}
			return jsonContent(map[string]any{"targets": targets})
		},
	}
}

// requireLease pulls lease_id out of args or errors.
func requireLease(args map[string]any) (string, error) {
	id := str(args, "lease_id")
	if id == "" {
		return "", fmt.Errorf("lease_id is required (call lease_acquire first)")
	}
	return id, nil
}
