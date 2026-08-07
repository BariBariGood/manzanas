package mcp

import (
	"context"
	"fmt"

	"github.com/BariBariGood/manzanas/proto"
)

// leaseTargetUDID resolves a lease's target for the target-scoped
// recording endpoints.
func leaseTargetUDID(ctx context.Context, s *Server, leaseID string) (string, error) {
	l, err := s.client.GetLease(ctx, leaseID)
	if err != nil {
		return "", err
	}
	if l.TargetUDID == "" {
		return "", fmt.Errorf("lease %s holds no target yet (state %s)", leaseID, l.State)
	}
	return l.TargetUDID, nil
}

func toolRecordStart() Tool {
	return Tool{
		Name:        "record_start",
		Description: "Start a screen recording of the leased target (mp4). One recording per target; it auto-stops at the daemon's duration/size caps and when the lease ends. Stop with record_stop to get the artifact.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id":    {"type": "string", "description": "active lease, from lease_acquire"},
			"codec":       {"type": "string", "enum": []string{"hevc", "h264"}, "description": "video codec; default hevc (smaller), use h264 for maximum player compatibility"},
			"max_seconds": {"type": "integer", "description": "duration cap in seconds; default/max is the daemon's configured cap"},
		}, "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			udid, err := leaseTargetUDID(ctx, s, leaseID)
			if err != nil {
				return nil, err
			}
			rec, err := s.client.StartRecording(ctx, udid, proto.RecordingRequest{
				LeaseID: leaseID, Codec: str(args, "codec"),
				MaxSeconds: int(num(args, "max_seconds")),
			})
			if err != nil {
				return nil, err
			}
			return jsonContent(rec)
		},
	}
}

func toolRecordStop() Tool {
	return Tool{
		Name:        "record_stop",
		Description: "Stop the leased target's screen recording; the finalized mp4 lands in the run journal (download via GET /v0/journal/{lease_id}/artifacts/{path} on the daemon, or `manzanas journal`).",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string", "description": "active lease, from lease_acquire"},
		}, "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			udid, err := leaseTargetUDID(ctx, s, leaseID)
			if err != nil {
				return nil, err
			}
			res, err := s.client.StopRecording(ctx, udid, leaseID)
			if err != nil {
				return nil, err
			}
			return jsonContent(res)
		},
	}
}
