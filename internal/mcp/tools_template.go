package mcp

import (
	"context"

	"github.com/BariBariGood/manzanas/proto"
)

// Template tools: the pixel-template fallback for elements the
// accessibility tree cannot see (matching half of the template library;
// save_template is exposed through the actions surface).

func toolTapTemplate() Tool {
	return Tool{
		Name:        "tap_template",
		Description: "Match a pixel template against the current screen and tap the match centre. Pass name (a template saved earlier with save_template, looked up per device class) or template_base64 (inline PNG/JPEG). Use it when an element has no accessibility presence (canvas buttons, hostile apps); prefer tap_element otherwise. Fails with the best score seen when nothing matches within timeout_ms.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string", "description": "Active lease ID from lease_acquire."},
			"name": {"type": "string",
				"description": "Name of a template saved with save_template (mutually exclusive with template_base64)."},
			"bundle_id": {"type": "string",
				"description": "Bundle the named template was saved under (default \"_global\")."},
			"template_base64": {"type": "string",
				"description": "Inline base64 PNG/JPEG template (mutually exclusive with name)."},
			"threshold": {"type": "number", "default": 0.8,
				"description": "Minimum normalized cross-correlation score in (0, 1] to count as a match."},
			"rect": {"type": "object",
				"description": "Optional {x,y,w,h} search region in capture-image pixels; the match must lie inside it."},
			"timeout_ms": {"type": "integer", "default": 3000,
				"description": "How long to keep re-capturing and matching before failing, in milliseconds (max 120000)."},
			"interval_ms": {"type": "integer", "default": 500,
				"description": "Delay between capture polls in milliseconds (min 10)."},
		}, "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			p := map[string]any{}
			for _, k := range []string{"name", "bundle_id", "template_base64", "threshold", "rect", "timeout_ms", "interval_ms"} {
				if v, ok := args[k]; ok {
					p[k] = v
				}
			}
			res, err := s.client.Dispatch(ctx, proto.ActionRequest{
				LeaseID: leaseID, Kind: "tap_template", Payload: p})
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
