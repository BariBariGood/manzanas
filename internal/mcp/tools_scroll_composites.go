// The list-scrolling composite tools: scroll_until and scroll_collect
// (one call instead of an agent round-trip per screen).
package mcp

import (
	"context"

	"github.com/BariBariGood/manzanas/proto"
)

// dispatchScrollComposite runs one list-scrolling composite and returns
// its result as-is: a target that never appeared is a soft
// {found: false} result, not an error, so no matcher hint applies.
func dispatchScrollComposite(ctx context.Context, s *Server, kind, leaseID string, payload map[string]any) ([]map[string]any, error) {
	res, err := s.client.Dispatch(ctx, proto.ActionRequest{
		LeaseID: leaseID, Kind: kind, Payload: payload})
	if err != nil {
		return nil, err
	}
	if err := actionErr(res); err != nil {
		return nil, err
	}
	return jsonContent(res.Result)
}

// scrollCompositeProps are the payload fields scroll_until and
// scroll_collect share.
func scrollCompositeProps() map[string]map[string]any {
	return map[string]map[string]any{
		"lease_id": {"type": "string", "description": "Active lease ID from lease_acquire."},
		"direction": {"type": "string", "enum": []string{"down", "up", "left", "right"}, "default": "down",
			"description": "Which content edge to reveal per gesture: down reveals content below the fold (default)."},
		"max_scrolls": {"type": "integer", "default": 8,
			"description": "Maximum gestures before giving up (max 30)."},
		"moved_jaccard": {"type": "number", "default": 0.6,
			"description": "Movement threshold: text-set Jaccard overlap at or above this counts as a non-move (bounce/end-of-list). In (0, 1]."},
		"timeout_ms": {"type": "integer", "default": 30000,
			"description": "Overall budget in milliseconds (max 120000)."},
		"interval_ms": {"type": "integer", "default": 500,
			"description": "Delay between observation polls in milliseconds (min 10)."},
	}
}

// scrollCompositePayload copies the shared and per-tool arguments into a
// daemon action payload (arg names match the payload fields 1:1).
func scrollCompositePayload(args map[string]any, extraKeys ...string) map[string]any {
	p := map[string]any{}
	keys := append([]string{"direction", "max_scrolls", "moved_jaccard",
		"timeout_ms", "interval_ms"}, extraKeys...)
	for _, k := range keys {
		if v, ok := args[k]; ok {
			p[k] = v
		}
	}
	return p
}

func toolScrollUntil() Tool {
	return Tool{
		Name:        "scroll_until",
		Description: "Scroll a list until the given text is visible, in one call — no round-trip per screen. Detects real movement vs bounce-back (masked text-set comparison) and stops at the end of the list. Not finding the text is a soft result: {found: false, end_of_list: ...}, never an error. Works on simulators, WDA devices, and mirror (OCR) devices.",
		InputSchema: schema(mergeProps(scrollCompositeProps(), map[string]map[string]any{
			"text": {"type": "string",
				"description": "Case-insensitive substring of the target's visible text (label, value, or placeholder)."},
		}), "lease_id", "text"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			if _, err := reqStr(args, "text"); err != nil {
				return nil, err
			}
			return dispatchScrollComposite(ctx, s, "scroll_until", leaseID,
				scrollCompositePayload(args, "text"))
		},
	}
}

func toolScrollCollect() Tool {
	return Tool{
		Name:        "scroll_collect",
		Description: "Scroll to the end of a list while harvesting every visible text box ({text, x, y, w, h}), deduplicated across screens — one call collects a whole list's content. Returns screens_texts (per-screen newly-seen boxes) plus collected, end_of_list, and moved_history.",
		InputSchema: schema(mergeProps(scrollCompositeProps(), map[string]map[string]any{
			"rect": {"type": "object",
				"description": "Optional region {x, y, w, h}: only boxes whose centre falls inside it are harvested."},
		}), "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			return dispatchScrollComposite(ctx, s, "scroll_collect", leaseID,
				scrollCompositePayload(args, "rect"))
		},
	}
}
