package mcp

import (
	"context"

	"github.com/BariBariGood/manzanas/proto"
)

// Deterministic sync tools: wait for an element or for the whole tree to
// settle instead of sleeping and hoping.

func toolWaitForElement() Tool {
	return Tool{
		Name:        "wait_for_element",
		Description: "Wait until an element matching the matcher appears (or, with absent=true, disappears). Returns the matched element with its frame. Use it after navigation or an action that loads content — never sleep and re-screenshot.",
		InputSchema: schema(mergeProps(matcherProps(), map[string]map[string]any{
			"absent": {"type": "boolean", "default": false,
				"description": "Invert the wait: succeed when NO element matches (e.g. a spinner or sheet went away)."},
		}), "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			hintKind := "wait_for_element"
			if absent, _ := args["absent"].(bool); absent {
				hintKind = "wait_for_element_absent"
			}
			res, err := s.client.Dispatch(ctx, proto.ActionRequest{
				LeaseID: leaseID, Kind: "wait_for_element",
				Payload: elementPayload(args, "absent")})
			if err != nil {
				return nil, matcherHint(hintKind, err)
			}
			if err := actionErr(res); err != nil {
				return nil, matcherHint(hintKind, err)
			}
			return jsonContent(res.Result)
		},
	}
}

func toolWaitTreeStable() Tool {
	return Tool{
		Name:        "wait_tree_stable",
		Description: "Wait until the screen stops changing (the accessibility tree hashes identically for several consecutive polls), i.e. animations and loads have settled. Returns stable (true/false) plus the final tree hash — compare hashes across calls to detect UI changes cheaply. A screen that never settles (looping animation) returns stable=false, which is an observation, not an error; prefer wait_for_element there.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string", "description": "Active lease ID from lease_acquire."},
			"stable_samples": {"type": "integer", "default": 3,
				"description": "How many consecutive identical tree snapshots count as settled (2-20)."},
			"timeout_ms": {"type": "integer", "default": 15000,
				"description": "Max wait in milliseconds (max 120000). Elapsing is not an error: the result carries stable=false."},
			"interval_ms": {"type": "integer", "default": 500,
				"description": "Delay between polls in milliseconds (min 10)."},
		}, "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			p := map[string]any{}
			for _, k := range []string{"stable_samples", "timeout_ms", "interval_ms"} {
				if v, ok := args[k]; ok {
					p[k] = v
				}
			}
			res, err := s.client.Dispatch(ctx, proto.ActionRequest{
				LeaseID: leaseID, Kind: "wait_tree_stable", Payload: p})
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
