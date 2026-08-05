package client

import (
	"context"
	"net/http"

	"github.com/BariBariGood/manzanas/proto"
)

// Dispatch calls POST /v0/actions with an opaque action payload. The
// payload schemas below are the contract with the actions slice.
func (c *Client) Dispatch(ctx context.Context, req proto.ActionRequest) (proto.ActionResult, error) {
	var res proto.ActionResult
	err := c.do(ctx, http.MethodPost, "/v0/actions", req, &res)
	return res, err
}

// Tap dispatches a "tap" action at (x, y) points.
func (c *Client) Tap(ctx context.Context, leaseID string, x, y float64) (proto.ActionResult, error) {
	return c.Dispatch(ctx, proto.ActionRequest{LeaseID: leaseID, Kind: "tap",
		Payload: map[string]any{"x": x, "y": y}})
}

// Swipe dispatches a "swipe" action between two points.
func (c *Client) Swipe(ctx context.Context, leaseID string, startX, startY, endX, endY float64, durationMS int) (proto.ActionResult, error) {
	p := map[string]any{"start_x": startX, "start_y": startY, "end_x": endX, "end_y": endY}
	if durationMS > 0 {
		p["duration_seconds"] = float64(durationMS) / 1000
	}
	return c.Dispatch(ctx, proto.ActionRequest{LeaseID: leaseID, Kind: "swipe", Payload: p})
}

// Type dispatches a "type" action entering text.
func (c *Client) Type(ctx context.Context, leaseID, text string) (proto.ActionResult, error) {
	return c.Dispatch(ctx, proto.ActionRequest{LeaseID: leaseID, Kind: "type",
		Payload: map[string]any{"text": text}})
}

// Button dispatches a "button" hardware-button press (home, lock, siri...).
func (c *Client) Button(ctx context.Context, leaseID, name string) (proto.ActionResult, error) {
	return c.Dispatch(ctx, proto.ActionRequest{LeaseID: leaseID, Kind: "button",
		Payload: map[string]any{"name": name}})
}

// Observe dispatches an "observe" action returning a compact a11y tree
// (result carries "tree" and "hash" per PROTOCOL.md §5.1).
func (c *Client) Observe(ctx context.Context, leaseID string) (proto.ActionResult, error) {
	return c.Dispatch(ctx, proto.ActionRequest{LeaseID: leaseID, Kind: "observe"})
}

// Screenshot dispatches a "screenshot" action with default encoding
// (PNG); the result carries base64-encoded image data (see ImageB64).
func (c *Client) Screenshot(ctx context.Context, leaseID string) (proto.ActionResult, error) {
	return c.ScreenshotOpts(ctx, leaseID, "", 0, 0)
}

// ScreenshotOpts dispatches a "screenshot" action with optional
// server-side encode controls (PROTOCOL.md §5.1): format ("png"|"jpeg"),
// JPEG quality (1–100) and max_dim (longer-edge pixel cap). Zero values
// are omitted, preserving the daemon's defaults.
func (c *Client) ScreenshotOpts(ctx context.Context, leaseID, format string, quality, maxDim int) (proto.ActionResult, error) {
	p := map[string]any{}
	if format != "" {
		p["format"] = format
	}
	if quality > 0 {
		p["quality"] = quality
	}
	if maxDim > 0 {
		p["max_dim"] = maxDim
	}
	if len(p) == 0 {
		p = nil
	}
	return c.Dispatch(ctx, proto.ActionRequest{LeaseID: leaseID, Kind: "screenshot", Payload: p})
}

// ImageB64 extracts base64 image data from a screenshot ActionResult:
// the key derived from the result's "format" field ("png_base64" /
// "jpeg_base64"), falling back to the known keys plus the generic
// "data_base64".
func ImageB64(res proto.ActionResult) string {
	if f, _ := res.Result["format"].(string); f != "" {
		if s, _ := res.Result[f+"_base64"].(string); s != "" {
			return s
		}
	}
	for _, key := range []string{"png_base64", "jpeg_base64", "data_base64"} {
		if s, _ := res.Result[key].(string); s != "" {
			return s
		}
	}
	return ""
}

// AppInstall dispatches an "install_app" action for a .app path
// reachable from the daemon host.
func (c *Client) AppInstall(ctx context.Context, leaseID, path string) (proto.ActionResult, error) {
	return c.Dispatch(ctx, proto.ActionRequest{LeaseID: leaseID, Kind: "install_app",
		Payload: map[string]any{"path": path}})
}

// AppLaunch dispatches a "launch_app" action by bundle ID.
func (c *Client) AppLaunch(ctx context.Context, leaseID, bundleID string) (proto.ActionResult, error) {
	return c.Dispatch(ctx, proto.ActionRequest{LeaseID: leaseID, Kind: "launch_app",
		Payload: map[string]any{"bundle_id": bundleID}})
}

// AppTerminate dispatches a "terminate_app" action by bundle ID.
func (c *Client) AppTerminate(ctx context.Context, leaseID, bundleID string) (proto.ActionResult, error) {
	return c.Dispatch(ctx, proto.ActionRequest{LeaseID: leaseID, Kind: "terminate_app",
		Payload: map[string]any{"bundle_id": bundleID}})
}
