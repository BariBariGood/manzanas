package wda

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Swipe drags from one point to another over duration seconds
// (POST /session/{id}/wda/dragfromtoforduration).
func (c *Client) Swipe(ctx context.Context, fromX, fromY, toX, toY, duration float64) error {
	_, err := c.inSession(ctx, http.MethodPost, "/wda/dragfromtoforduration", map[string]any{
		"fromX": fromX, "fromY": fromY, "toX": toX, "toY": toY,
		"duration": duration,
	})
	return err
}

// PressButton presses a hardware button (POST /session/{id}/wda/pressButton).
// WDA accepts "home", "volumeUp", and "volumeDown".
func (c *Client) PressButton(ctx context.Context, name string) error {
	_, err := c.inSession(ctx, http.MethodPost, "/wda/pressButton", map[string]any{"name": name})
	return err
}

// Lock locks the device screen (POST /session/{id}/wda/lock).
func (c *Client) Lock(ctx context.Context) error {
	_, err := c.inSession(ctx, http.MethodPost, "/wda/lock", map[string]any{})
	return err
}

// SetPasteboard writes plaintext to the device pasteboard
// (POST /session/{id}/wda/setPasteboard).
func (c *Client) SetPasteboard(ctx context.Context, text string) error {
	_, err := c.inSession(ctx, http.MethodPost, "/wda/setPasteboard", map[string]any{
		"content":     base64.StdEncoding.EncodeToString([]byte(text)),
		"contentType": "plaintext",
	})
	return err
}

// GetPasteboard reads plaintext from the device pasteboard
// (POST /session/{id}/wda/getPasteboard). WDA requires the WDA app itself
// to be foregrounded on iOS 13+ for pasteboard reads; a failure surfaces
// as a protocol Error.
func (c *Client) GetPasteboard(ctx context.Context) (string, error) {
	val, err := c.inSession(ctx, http.MethodPost, "/wda/getPasteboard", map[string]any{
		"contentType": "plaintext",
	})
	if err != nil {
		return "", err
	}
	var b64 string
	if err := json.Unmarshal(val, &b64); err != nil {
		return "", fmt.Errorf("wda: getPasteboard value is not a string: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		// Some WDA builds return the raw string un-encoded.
		return b64, nil
	}
	return string(decoded), nil
}
