package mcp

import (
	"context"
	"errors"

	"github.com/BariBariGood/manzanas/internal/client"
)

func toolObserve() Tool {
	return Tool{
		Name:        "observe",
		Description: "Get a compact accessibility tree of the leased simulator's screen (element labels + frames). Use before tapping.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string"},
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
		Description: "Tap the screen at (x, y) points.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string"},
			"x":        {"type": "number"},
			"y":        {"type": "number"},
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
		Description: "Swipe from (start_x, start_y) to (end_x, end_y); optional duration_ms.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id":    {"type": "string"},
			"start_x":     {"type": "number"},
			"start_y":     {"type": "number"},
			"end_x":       {"type": "number"},
			"end_y":       {"type": "number"},
			"duration_ms": {"type": "integer"},
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
		Description: "Type text into the focused field.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string"},
			"text":     {"type": "string"},
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
		Description: "Press a hardware button: home, lock, side-button, siri, apple-pay.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string"},
			"name":     {"type": "string"},
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
		Description: "Capture the leased simulator's screen as a PNG image.",
		InputSchema: schema(map[string]map[string]any{
			"lease_id": {"type": "string"},
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
