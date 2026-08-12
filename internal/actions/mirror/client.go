// Package mirror is a pure-Go client for the mirrord helper — the small
// GUI-session process that drives macOS iPhone Mirroring with CGEvents,
// screencapture, and Vision OCR (helpers/mirrord). manzanasd talks to it
// over a local unix socket; the helper owns everything that needs TCC
// grants (Accessibility, Screen Recording) and an active GUI session,
// which a daemon started over SSH does not have.
//
// The mirror is exclusive global state: one phone per Mac, and input is
// swallowed unless the mirroring window is frontmost. The client
// serializes all calls so concurrent actions never interleave gestures.
package mirror

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// DefaultTimeout bounds each helper round-trip. OCR of a busy screen
// takes a couple of seconds; gestures (a slow swipe) up to a few more.
const DefaultTimeout = 60 * time.Second

// Client talks to one mirrord helper over its unix socket.
type Client struct {
	socket string
	httpc  *http.Client

	// mu serializes helper calls: the mirroring window is one shared
	// physical resource and interleaved gestures corrupt each other.
	mu sync.Mutex
}

// New returns a Client for a mirrord unix socket path.
func New(socketPath string) *Client {
	return &Client{
		socket: socketPath,
		httpc: &http.Client{
			Timeout: DefaultTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// Socket returns the configured socket path.
func (c *Client) Socket() string { return c.socket }

// Error is a structured helper failure. Code is one of the helper's
// stable error codes: "not-running" (iPhone Mirroring app not open),
// "no-window" (app open, no phone window), "blocked" (a connect /
// "iPhone in Use" / paused interstitial is on screen), "capture-failed",
// "untypable" (a character with no HID keycode), "bad-request".
type Error struct {
	Code    string `json:"code"`
	Message string `json:"error"`
}

func (e *Error) Error() string { return "mirrord " + e.Code + ": " + e.Message }

// Status is the helper's view of the mirroring session.
type Status struct {
	// State is "ready", "blocked", "no-window", or "not-running".
	State string `json:"state"`
	// Frontmost reports whether the mirroring window currently has focus
	// (input is swallowed when it does not; the helper activates it
	// before every input, so false here is informational).
	Frontmost bool `json:"frontmost"`
	// Window is the mirroring window's screen-point bounds, when present.
	Window *Window `json:"window,omitempty"`
	// Marker is the interstitial text that flagged a blocked state.
	Marker string `json:"marker,omitempty"`
}

// Window is the mirroring window's bounds in Mac screen points.
type Window struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// OCRBox is one recognized text run. Coordinates are pixels in the
// capture image — the same space Screenshot returns and Tap accepts.
type OCRBox struct {
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	W          float64 `json:"w"`
	H          float64 `json:"h"`
}

// Screen is a capture: PNG bytes plus its pixel dimensions, the shared
// coordinate space for OCR boxes and input.
type Screen struct {
	PNG  []byte
	ImgW int
	ImgH int
}

// OCRResult is a capture's recognized text plus the capture dimensions.
type OCRResult struct {
	Boxes []OCRBox
	ImgW  int
	ImgH  int
}

func (c *Client) call(ctx context.Context, method, path string, req, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var body io.Reader
	if req != nil {
		b, err := json.Marshal(req)
		if err != nil {
			return fmt.Errorf("mirrord: encode %s: %w", path, err)
		}
		body = bytes.NewReader(b)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, "http://mirrord"+path, body)
	if err != nil {
		return err
	}
	if req != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpc.Do(hreq)
	if err != nil {
		return fmt.Errorf("mirrord unreachable at %s: %w", c.socket, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("mirrord: read %s: %w", path, err)
	}
	if resp.StatusCode/100 != 2 {
		var he Error
		if json.Unmarshal(data, &he) == nil && he.Code != "" {
			return &he
		}
		return fmt.Errorf("mirrord %s: HTTP %d: %s", path, resp.StatusCode, bytes.TrimSpace(data))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("mirrord: decode %s: %w", path, err)
	}
	return nil
}

// Status reports the mirroring session state without touching input.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.call(ctx, http.MethodGet, "/v1/status", nil, &s)
	return s, err
}

// Tap taps at capture-image pixel coordinates.
func (c *Client) Tap(ctx context.Context, x, y float64) error {
	return c.call(ctx, http.MethodPost, "/v1/tap", map[string]any{"x": x, "y": y}, nil)
}

// LongPress holds a press at (x, y) for the given duration.
func (c *Client) LongPress(ctx context.Context, x, y float64, d time.Duration) error {
	return c.call(ctx, http.MethodPost, "/v1/tap",
		map[string]any{"x": x, "y": y, "duration_ms": d.Milliseconds()}, nil)
}

// Swipe drags from (x1, y1) to (x2, y2) over the given duration. A fast
// short drag is a momentum flick; a slow one barely moves an iOS list.
func (c *Client) Swipe(ctx context.Context, x1, y1, x2, y2 float64, d time.Duration) error {
	return c.call(ctx, http.MethodPost, "/v1/swipe", map[string]any{
		"x1": x1, "y1": y1, "x2": x2, "y2": y2, "duration_ms": d.Milliseconds()}, nil)
}

// Scroll posts wheel-scroll events at (x, y). Positive dy scrolls content
// down (like a trackpad two-finger-up); units are capture-image pixels.
func (c *Client) Scroll(ctx context.Context, x, y, dy float64) error {
	return c.call(ctx, http.MethodPost, "/v1/scroll",
		map[string]any{"x": x, "y": y, "dy": dy}, nil)
}

// Type types text via raw HID keycodes (US layout). iPhone Mirroring
// forwards keycodes, not unicode payloads, so characters outside the
// keycode map fail with code "untypable".
func (c *Client) Type(ctx context.Context, text string) error {
	return c.call(ctx, http.MethodPost, "/v1/type", map[string]any{"text": text}, nil)
}

// Press sends an iPhone Mirroring shortcut combo: "cmd+1" (Home),
// "cmd+2" (App Switcher), or "cmd+3" (Spotlight). The helper rejects
// anything else — other combos would drive the host Mac.
func (c *Client) Press(ctx context.Context, combo string) error {
	return c.call(ctx, http.MethodPost, "/v1/press", map[string]any{"combo": combo}, nil)
}

// Screenshot captures the mirroring window as PNG.
func (c *Client) Screenshot(ctx context.Context) (Screen, error) {
	var raw struct {
		PNGBase64 string `json:"png_base64"`
		ImgW      int    `json:"img_w"`
		ImgH      int    `json:"img_h"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/screenshot", nil, &raw); err != nil {
		return Screen{}, err
	}
	png, err := base64.StdEncoding.DecodeString(raw.PNGBase64)
	if err != nil {
		return Screen{}, fmt.Errorf("mirrord: bad screenshot payload: %w", err)
	}
	return Screen{PNG: png, ImgW: raw.ImgW, ImgH: raw.ImgH}, nil
}

// OCR captures the window and runs Vision text recognition over it.
func (c *Client) OCR(ctx context.Context) (OCRResult, error) {
	var raw struct {
		Boxes []OCRBox `json:"boxes"`
		ImgW  int      `json:"img_w"`
		ImgH  int      `json:"img_h"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/ocr", nil, &raw); err != nil {
		return OCRResult{}, err
	}
	return OCRResult{Boxes: raw.Boxes, ImgW: raw.ImgW, ImgH: raw.ImgH}, nil
}
