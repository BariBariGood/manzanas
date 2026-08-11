package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/BariBariGood/manzanas/internal/client"
	"github.com/BariBariGood/manzanas/proto"
)

func toolLeaseAcquire() Tool {
	return Tool{
		Name:        "lease_acquire",
		Description: "Claim an iOS simulator or physical device matching labels (e.g. [\"ios26\",\"iphone-17-pro\"]; use [\"device\"] for a physical device). Returns lease_id + target_udid; every other tool needs the lease_id. By default a Shutdown target is booted before this returns (boot=false skips that). Waits in a queue if all matching targets are busy. Call this FIRST, and lease_release when done.",
		InputSchema: schema(map[string]map[string]any{
			"labels":      {"type": "array", "items": map[string]any{"type": "string"}, "description": "target labels to match (see the targets tool for the available labels); empty = any target"},
			"udid":        {"type": "string", "description": "pin the lease to this exact target UDID instead of label matching"},
			"ttl_seconds": {"type": "integer", "description": "lease time-to-live in seconds; default 300, max 3600. Renew before expiry with lease_renew (expired leases keep working through a short renewal grace window)"},
			"wait":        {"type": "boolean", "description": "if all matching targets are busy, wait in the queue until one frees up (default true); false returns a queued lease immediately"},
			"boot":        {"type": "boolean", "description": "boot the leased target if it is not already Booted and wait until it is ready (default true); false returns as soon as the lease is granted. If the boot fails the lease is still returned, with a boot_error field explaining what happened"},
			"agent_id":    {"type": "string", "description": "identity recorded on the lease and in the run journal (default \"manzanas-mcp\"); set it so multi-agent runs are attributable"},
			"purpose":     {"type": "string", "description": "free-text purpose recorded on the lease and in the run journal (e.g. \"login flow QA\")"},
			"reset":       {"type": "string", "description": "auto-reset the target when the lease ends: \"none\" (default), \"erase\", or \"snapshot:<name>\". After an erase the host may report overloaded for a minute or two before new boots are admitted"},
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
			agentID := str(args, "agent_id")
			if agentID == "" {
				agentID = "manzanas-mcp"
			}
			l, err := s.client.AcquireLease(ctx, proto.AcquireLeaseRequest{
				Labels: labels, UDID: str(args, "udid"), AgentID: agentID,
				Purpose:    str(args, "purpose"),
				Reset:      str(args, "reset"),
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
			boot := true
			if b, ok := args["boot"].(bool); ok {
				boot = b
			}
			if boot && l.State == proto.LeaseActive {
				if err := bootLeasedTarget(ctx, s, l); err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					// The lease WAS granted: return it (so the agent can use
					// or release it) with a boot warning instead of an error
					// that would hide the held lease and invite re-acquires.
					return leaseWithBootError(l, err)
				}
			}
			return jsonContent(l)
		},
	}
}

// bootWaitPoll/bootWaitBudget bound the wait for a leased target to reach
// Booted after the boot request is accepted (the boot itself is async).
var (
	bootWaitPoll   = 2 * time.Second
	bootWaitBudget = 5 * time.Minute
)

// bootLeasedTarget makes a fresh lease usable end-to-end: if the leased
// simulator is not Booted it requests a boot (server-side wait absorbs
// overload back-pressure) and polls until the target reports Booted.
// Physical devices are never booted (the daemon cannot boot them).
func bootLeasedTarget(ctx context.Context, s *Server, l proto.Lease) error {
	tgt, err := leasedTarget(ctx, s, l)
	if err != nil {
		return err
	}
	if tgt.Kind == proto.TargetDevice || tgt.State == proto.StateBooted {
		return nil
	}
	if _, err := s.client.BootTargetWait(ctx, l.TargetUDID, l.ID); err != nil {
		if endpointLacksBoot(err) {
			// The endpoint has no boot route (e.g. a manzanas-broker
			// fleet endpoint) or cannot boot this target: the lease was
			// granted, so return it rather than failing the acquire.
			return nil
		}
		return err
	}
	deadline := time.Now().Add(bootWaitBudget)
	for {
		tgt, err = leasedTarget(ctx, s, l)
		if err != nil {
			return err
		}
		if tgt.State == proto.StateBooted {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("target %s still %s after waiting %s for boot",
				l.TargetUDID, tgt.State, bootWaitBudget)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(bootWaitPoll):
		}
	}
}

// leaseWithBootError emits the granted lease plus a boot_error field so a
// boot problem never masks the fact that the lease is active and held.
func leaseWithBootError(l proto.Lease, bootErr error) ([]map[string]any, error) {
	b, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	m["boot_error"] = fmt.Sprintf("%v. The lease is active and held; the target may "+
		"still be booting \u2014 retry your tool calls, or lease_release and "+
		"lease_acquire a different target", bootErr)
	return jsonContent(m)
}

// endpointLacksBoot reports whether a boot request failed because the
// endpoint does not serve target lifecycle routes at all (a broker's mux
// answers a plain non-JSON 404, which the client surfaces without a
// proto error code) or explicitly does not implement boot (501). A JSON
// 404 with code not_found is a genuine lease/target-not-found failure
// from a real daemon and is NOT tolerated.
func endpointLacksBoot(err error) bool {
	var ae *client.APIError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.Status == http.StatusNotImplemented {
		return true
	}
	return ae.Status == http.StatusNotFound && ae.Code != proto.ErrNotFound
}

// leasedTarget finds the lease's target in the target list. Against a
// broker the list is a fleet-wide union in which the same UDID can appear
// on several hosts, so when the lease carries a host annotation only that
// host's entry counts.
func leasedTarget(ctx context.Context, s *Server, l proto.Lease) (proto.Target, error) {
	targets, err := s.client.ListTargets(ctx)
	if err != nil {
		return proto.Target{}, err
	}
	for _, t := range targets {
		if t.UDID == l.TargetUDID && (l.Host == "" || t.Host == "" || t.Host == l.Host) {
			return t, nil
		}
	}
	return proto.Target{}, fmt.Errorf("leased target %s not found in the target list", l.TargetUDID)
}

func toolLeaseRelease() Tool {
	return Tool{
		Name:        "lease_release",
		Description: "Release a lease when done so other agents can use the target. Leases you acquired in this session are also auto-released when the session ends.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string", "description": "the lease to release, from lease_acquire"},
		}, "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			id, err := requireLease(args)
			if err != nil {
				return nil, err
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

func toolLeaseRenew() Tool {
	return Tool{
		Name:        "lease_renew",
		Description: "Extend an active lease's TTL before it expires. A renew shortly after nominal expiry may still rescue the lease (the daemon allows a short grace window), but do not rely on it — once the grace passes, acquire a new lease with lease_acquire.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id":    {"type": "string", "description": "the lease to renew, from lease_acquire"},
			"ttl_seconds": {"type": "integer", "description": "new time-to-live in seconds from now; default 300, max 3600"},
		}, "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			id, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			l, err := s.client.RenewLease(ctx, id, int(num(args, "ttl_seconds")))
			if err != nil {
				return nil, err
			}
			return jsonContent(l)
		},
	}
}

func toolTargets() Tool {
	return Tool{
		Name:        "targets",
		Description: "List simulators and physical devices in the fleet: udid, kind, name, runtime, state, labels. Physical devices have kind \"device\" (plus a \"disconnected\" label when their tunnel is down).",
		InputSchema: schema(map[string]map[string]any{
			"kind": {"type": "string", "enum": []string{"simulator", "device"}, "description": "only list targets of this kind; omit for all"},
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
