package mcp

import (
	"context"
	"errors"

	"github.com/BariBariGood/manzanas/internal/client"
)

func toolObserve() Tool {
	return Tool{
		Name:        "observe",
		Description: "Alias of ui_tree, kept for backward compatibility — prefer ui_tree. Gets a compact accessibility tree of the leased target's screen: element roles, labels, and frames ({x, y, w, h} in screen points, origin top-left).",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string", "description": "active lease, from lease_acquire"},
		}, "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			res, err := s.client.Observe(ctx, leaseID)
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

func toolTap() Tool {
	return Tool{
		Name:        "tap",
		Description: "Tap the screen at (x, y). Coordinates are screen points (origin top-left, same units as the frames returned by observe) — aim for the center of an element's frame.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string", "description": "active lease, from lease_acquire"},
			"x":        {"type": "number", "description": "horizontal position in screen points from the left edge"},
			"y":        {"type": "number", "description": "vertical position in screen points from the top edge"},
		}, "lease_id", "x", "y"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			x, err := reqNum(args, "x")
			if err != nil {
				return nil, err
			}
			y, err := reqNum(args, "y")
			if err != nil {
				return nil, err
			}
			res, err := s.client.Tap(ctx, leaseID, x, y)
			if err != nil {
				return nil, err
			}
			return actionContent(res)
		},
	}
}

func toolSwipe() Tool {
	return Tool{
		Name:        "swipe",
		Description: "Swipe from (start_x, start_y) to (end_x, end_y) in screen points. To scroll content down, swipe upward (end_y < start_y).",
		InputSchema: schema(map[string]map[string]any{
			"lease_id":    {"type": "string", "description": "active lease, from lease_acquire"},
			"start_x":     {"type": "number", "description": "start position, screen points from the left edge"},
			"start_y":     {"type": "number", "description": "start position, screen points from the top edge"},
			"end_x":       {"type": "number", "description": "end position, screen points from the left edge"},
			"end_y":       {"type": "number", "description": "end position, screen points from the top edge"},
			"duration_ms": {"type": "integer", "description": "gesture duration in milliseconds; longer = slower swipe (daemon default if omitted)"},
		}, "lease_id", "start_x", "start_y", "end_x", "end_y"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			var coords [4]float64
			for i, key := range []string{"start_x", "start_y", "end_x", "end_y"} {
				v, err := reqNum(args, key)
				if err != nil {
					return nil, err
				}
				coords[i] = v
			}
			res, err := s.client.Swipe(ctx, leaseID,
				coords[0], coords[1], coords[2], coords[3], int(num(args, "duration_ms")))
			if err != nil {
				return nil, err
			}
			return actionContent(res)
		},
	}
}

func toolType() Tool {
	return Tool{
		Name:        "type_text",
		Description: "Type text into the currently focused text field. Tap the field first to give it keyboard focus; fails with focus_required if nothing is focused.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string", "description": "active lease, from lease_acquire"},
			"text":     {"type": "string", "description": "the text to type, sent as keystrokes"},
		}, "lease_id", "text"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			text, err := reqStr(args, "text")
			if err != nil {
				return nil, err
			}
			res, err := s.client.Type(ctx, leaseID, text)
			if err != nil {
				return nil, err
			}
			return actionContent(res)
		},
	}
}

func toolButton() Tool {
	return Tool{
		Name:        "button",
		Description: "Press a hardware button on the leased target (e.g. home to go to the home screen, lock to lock the screen).",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string", "description": "active lease, from lease_acquire"},
			"name":     {"type": "string", "enum": []string{"home", "lock", "side-button", "siri", "apple-pay"}, "description": "which hardware button to press"},
		}, "lease_id", "name"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			name, err := reqStr(args, "name")
			if err != nil {
				return nil, err
			}
			res, err := s.client.Button(ctx, leaseID, name)
			if err != nil {
				return nil, err
			}
			return actionContent(res)
		},
	}
}

func toolScreenshot() Tool {
	return Tool{
		Name:        "screenshot",
		Description: "Capture the leased target's screen as a PNG image. Prefer observe for locating elements; use screenshots to verify visual state.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string", "description": "active lease, from lease_acquire"},
		}, "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			res, err := s.client.Screenshot(ctx, leaseID)
			if err != nil {
				return nil, err
			}
			if err := actionErr(res); err != nil {
				return nil, err
			}
			b64 := client.ImageB64(res)
			if b64 == "" {
				return nil, errors.New("daemon returned no image data")
			}
			return []map[string]any{
				{"type": "image", "data": b64, "mimeType": "image/png"},
			}, nil
		},
	}
}
