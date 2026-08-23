// Package mirror is a pure-Go client for the mirrord helper — the small
// GUI-session process that drives macOS iPhone Mirroring with CGEvents,
// screencapture, and Vision OCR (helpers/mirrord). manzanasd talks to it
// over a local unix socket; the helper owns everything that needs TCC
// grants (Accessibility, Screen Recording) and an active GUI session,
// which a daemon started over SSH does not have.
//
// The mirror is exclusive global state: one phone per Mac, so the client
// serializes all calls and concurrent actions never interleave gestures.
// Input reaches the phone either focus-free (the helper posts CGS event
// records addressed to the mirroring process) or, as a fallback, by
// activating the window and posting at the HID tap — Status.InputMode
// says which, and every input answers with an Ack carrying the evidence
// that it landed.
package mirror

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
// "iPhone in Use" / paused interstitial is on screen), "focus-lost"
// (the mirroring window could not be brought frontmost, so input was
// not sent — the message names the app holding focus), "capture-failed",
// "untypable" (a character with no HID keycode), "bad-request",
// "input-not-delivered" (the events were posted but the phone did not
// change, checked by capture delta — retryable), "scroll-not-delivered"
// (the same verdict for a background wheel scroll the helper could not
// re-deliver focused, because --background=on forbids that).
type Error struct {
	Code    string `json:"code"`
	Message string `json:"error"`
}

func (e *Error) Error() string { return "mirrord " + e.Code + ": " + e.Message }

// Status is the helper's view of the mirroring session.
type Status struct {
	// State is "ready", "blocked", "no-window", or "not-running".
	State string `json:"state"`
	// Frontmost reports whether the mirroring window currently has focus.
	// Informational: in background mode input does not need it, and in
	// focused mode the helper activates the window before every input.
	Frontmost bool `json:"frontmost"`
	// Background reports whether input can be delivered without taking
	// the host Mac's focus. False on helpers predating the field.
	Background bool `json:"background"`
	// InputMode is the mode the next action will use: "background"
	// (focus-free CGS event records) or "focused" (activate the window,
	// post at the HID tap). Empty on helpers predating the field.
	InputMode string `json:"input_mode"`
	// BackgroundScroll reports whether wheel scrolls can be posted on
	// the background path at all (symbol probe). Delivery there is
	// best-effort: the helper verifies each scroll and falls back to the
	// focused path on a miss. False on helpers predating the field.
	BackgroundScroll bool `json:"background_scroll"`
	// Capture is the active capture backend: "sck" (ScreenCaptureKit),
	// "cgwindowlist", "screencapture" (subprocess fallback), or "none".
	// Empty on helpers predating the field.
	Capture string `json:"capture"`
	// Verify is how actions are verified: "pixel-delta" or "off".
	Verify string `json:"verify"`
	// BackgroundBlocker explains why input_mode is "focused", when it is.
	BackgroundBlocker string `json:"background_blocker,omitempty"`
	// Window is the mirroring window's screen-point bounds, when present.
	Window *Window `json:"window,omitempty"`
	// Marker is the interstitial text that flagged a blocked state.
	Marker string `json:"marker,omitempty"`
}

// Ack is an input response: which mode delivered the events and whether
// the helper has evidence they landed on the phone.
type Ack struct {
	OK bool `json:"ok"`
	// Verified is true only when the helper saw the phone change.
	Verified bool `json:"verified"`
	// Verify is how it checked: "pixel-delta" (capture before/after),
	// "off" (verification disabled), "unavailable" (no usable capture).
	// Empty on helpers predating verification — treat as unverified, not
	// as failure.
	Verify string `json:"verify"`
	// Mode is "background", "focused", or "focused-degrade" (a
	// background-mode type delivered by briefly activating the window:
	// iOS routes key events into text input only while iPhone Mirroring
	// is frontmost); empty on older helpers.
	Mode string `json:"mode"`
	// Delta is the fraction of pixels that changed in the touched region.
	Delta float64 `json:"delta,omitempty"`
	// Note explains a per-action deviation, e.g. scroll falling back to
	// the focused path.
	Note string `json:"note,omitempty"`
	// DurationMS is the press/drag duration the helper actually ran,
	// after its own clamp; 0 on helpers predating the echo.
	DurationMS float64 `json:"duration_ms,omitempty"`
}

// Unverified reports an action the helper could not vouch for: it either
// checked and saw nothing ("pixel-delta"), or lost the ability to check
// mid-action ("unavailable"). Verification switched off at the helper
// ("off") and helpers predating the field ("") make no claim either way,
// so they are not unverified in this sense.
func (a Ack) Unverified() bool {
	return !a.Verified && a.Verify != "" && a.Verify != "off"
}

// DoctorCheck is one capability probe from GET /v1/doctor.
type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// DoctorReport is the helper's capability report: probes (a real capture,
// a real event record) rather than TCC database answers, which are wrong
// in both directions on real Macs.
type DoctorReport struct {
	OK               bool          `json:"ok"`
	InputMode        string        `json:"input_mode"`
	Background       bool          `json:"background"`
	BackgroundScroll bool          `json:"background_scroll"`
	Capture          string        `json:"capture"`
	Verify           string        `json:"verify"`
	State            string        `json:"state"`
	Checks           []DoctorCheck `json:"checks"`
}

// Window is the mirroring window's bounds in Mac screen points.
type Window struct {
	// ID is the CGWindowID of the selected mirroring window, for
	// diagnosing wrong-window selection. Zero when the helper predates
	// the field.
	ID int64   `json:"window_id,omitempty"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
	W  float64 `json:"w"`
	H  float64 `json:"h"`
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
// coordinate space for OCR boxes and input. Scale is the capture's
// pixels-per-window-point factor (the host display's backing factor);
// 0 when the helper predates the field.
type Screen struct {
	PNG   []byte
	ImgW  int
	ImgH  int
	Scale float64
}

// OCRRect is a recognition region of interest in capture-image pixels
// (top-left origin) — the same space Screenshot reports and Tap accepts.
type OCRRect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// OCROptions tunes one recognition pass. The zero value is the legacy
// behavior — accurate recognition over the full frame, no downscale —
// and is sent as a bare GET so older helpers keep working.
type OCROptions struct {
	// Level is "fast" or "accurate" (Vision recognition level).
	// Empty means the helper default, accurate.
	Level string
	// Rect restricts recognition to a region; boxes are still reported
	// in full-capture pixels.
	Rect *OCRRect
	// MaxDim caps the capture's long edge (pixels) before recognition.
	// The helper never downscales frames at or below 640px wide, so
	// ~318px mirror frames are never degraded. 0 means no cap.
	MaxDim int
}

func (o OCROptions) zero() bool {
	return o.Level == "" && o.Rect == nil && o.MaxDim == 0
}

// OCRResult is a capture's recognized text plus the capture dimensions
// and the recognition settings the helper actually applied.
type OCRResult struct {
	Boxes []OCRBox
	ImgW  int
	ImgH  int
	// Scale is the capture's pixels-per-window-point factor; 0 when
	// the helper predates the field.
	Scale float64
	// ServedScale is the resolution served to recognition relative to
	// the capture (1.0 = full resolution).
	ServedScale float64
	// Level is the recognition level applied ("fast" or "accurate").
	Level string
	// Rect echoes the region of interest, when one was requested.
	Rect *OCRRect
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
// The helper runs a full window capture + OCR pass to detect blocked
// interstitials, so this is expensive on its single serial worker —
// liveness pollers should prefer StatusLight.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.call(ctx, http.MethodGet, "/v1/status", nil, &s)
	return s, err
}

// StatusLight reports the mirroring session state without the helper's
// capture+OCR interstitial check: process/window/state presence only, so
// a blocked session reports "ready". Cheap enough for liveness polling.
// Helpers predating the light mode answer the query path with a
// "bad-request" route error; callers should fall back to Status then.
func (c *Client) StatusLight(ctx context.Context) (Status, error) {
	var s Status
	err := c.call(ctx, http.MethodGet, "/v1/status?light=1", nil, &s)
	return s, err
}

// IsUnsupportedRoute reports whether err is the helper's "no such route"
// answer, i.e. the helper predates the requested route or mode.
func IsUnsupportedRoute(err error) bool {
	var he *Error
	return errors.As(err, &he) && he.Code == "bad-request"
}

// Doctor reports the helper's probed capabilities. Older helpers have no
// such route and answer with a "bad-request" error.
func (c *Client) Doctor(ctx context.Context) (DoctorReport, error) {
	var r DoctorReport
	err := c.call(ctx, http.MethodGet, "/v1/doctor", nil, &r)
	return r, err
}

// The *Ack methods are the input API: they return the helper's evidence
// (mode + verification) alongside the error. The plain error-returning
// forms below wrap them for callers that only care whether the gesture
// was accepted.

// TapAck taps at capture-image pixel coordinates.
func (c *Client) TapAck(ctx context.Context, x, y float64) (Ack, error) {
	return c.input(ctx, "/v1/tap", map[string]any{"x": x, "y": y})
}

// LongPressAck holds a press at (x, y) for the given duration.
func (c *Client) LongPressAck(ctx context.Context, x, y float64, d time.Duration) (Ack, error) {
	return c.input(ctx, "/v1/tap",
		map[string]any{"x": x, "y": y, "duration_ms": d.Milliseconds()})
}

// SwipeAck drags from (x1, y1) to (x2, y2) over the given duration. A
// fast short drag is a momentum flick; a slow one barely moves an iOS
// list.
func (c *Client) SwipeAck(ctx context.Context, x1, y1, x2, y2 float64, d time.Duration) (Ack, error) {
	return c.input(ctx, "/v1/swipe", map[string]any{
		"x1": x1, "y1": y1, "x2": x2, "y2": y2, "duration_ms": d.Milliseconds()})
}

// ScrollAck posts wheel-scroll events at (x, y). Positive dy scrolls
// content down (like a trackpad two-finger-up); units are capture-image
// pixels. On helpers with background scroll (Status.BackgroundScroll)
// the Ack reports mode "background"; delivery there is best-effort, so
// the helper verifies each scroll and falls back to the focused path on
// a miss (the Ack's Note says so). Older helpers always report mode
// "focused".
func (c *Client) ScrollAck(ctx context.Context, x, y, dy float64) (Ack, error) {
	return c.input(ctx, "/v1/scroll", map[string]any{"x": x, "y": y, "dy": dy})
}

// TypeAck types text via raw HID keycodes (US layout). iPhone Mirroring
// forwards keycodes, not unicode payloads, so characters outside the
// keycode map fail with code "untypable".
func (c *Client) TypeAck(ctx context.Context, text string) (Ack, error) {
	return c.input(ctx, "/v1/type", map[string]any{"text": text})
}

// PressAck sends an iPhone Mirroring shortcut combo: "cmd+1" (Home),
// "cmd+2" (App Switcher), or "cmd+3" (Spotlight). The helper rejects
// anything else — other combos would drive the host Mac.
func (c *Client) PressAck(ctx context.Context, combo string) (Ack, error) {
	return c.input(ctx, "/v1/press", map[string]any{"combo": combo})
}

func (c *Client) input(ctx context.Context, path string, req map[string]any) (Ack, error) {
	var ack Ack
	err := c.call(ctx, http.MethodPost, path, req, &ack)
	return ack, err
}

// Refused reports an input failure where the helper sent nothing to the
// phone: the configuration refusal ("input-refused", --background=on
// forbids the only deliverable path) or a focused-path focus loss
// ("focus-lost", another app held the Mac's focus so no events were
// posted). Because no events went out, a caller may safely deliver the
// intent through a different gesture without risking a double input.
func Refused(err error) bool {
	var me *Error
	if !errors.As(err, &me) {
		return false
	}
	return me.Code == "input-refused" || me.Code == "focus-lost"
}

// UnverifiedError reports an input the helper accepted but could not
// vouch for — the honesty contract for callers of the error-only
// wrappers, which have no result map to stamp `verified` onto. The Ack
// is carried whole so the caller can distinguish a completed negative
// check from one that could not run.
type UnverifiedError struct {
	Ack Ack
}

func (e *UnverifiedError) Error() string {
	return fmt.Sprintf("mirrord: input posted in %s mode but unverified (verify %s)",
		e.Ack.Mode, e.Ack.Verify)
}

// ackOnly folds an acknowledgement into the error-only interface: an
// unverified ack becomes a typed error, so discarding the Ack never
// discards the evidence contract.
func ackOnly(ack Ack, err error) error {
	if err != nil {
		return err
	}
	if ack.Unverified() {
		return &UnverifiedError{Ack: ack}
	}
	return nil
}

// Tap taps at capture-image pixel coordinates.
func (c *Client) Tap(ctx context.Context, x, y float64) error {
	return ackOnly(c.TapAck(ctx, x, y))
}

// LongPress holds a press at (x, y) for the given duration.
func (c *Client) LongPress(ctx context.Context, x, y float64, d time.Duration) error {
	return ackOnly(c.LongPressAck(ctx, x, y, d))
}

// Swipe drags from (x1, y1) to (x2, y2) over the given duration.
func (c *Client) Swipe(ctx context.Context, x1, y1, x2, y2 float64, d time.Duration) error {
	return ackOnly(c.SwipeAck(ctx, x1, y1, x2, y2, d))
}

// Scroll posts wheel-scroll events at (x, y).
func (c *Client) Scroll(ctx context.Context, x, y, dy float64) error {
	return ackOnly(c.ScrollAck(ctx, x, y, dy))
}

// Type types text via raw HID keycodes (US layout).
func (c *Client) Type(ctx context.Context, text string) error {
	return ackOnly(c.TypeAck(ctx, text))
}

// Press sends an iPhone Mirroring shortcut combo.
func (c *Client) Press(ctx context.Context, combo string) error {
	return ackOnly(c.PressAck(ctx, combo))
}

// Screenshot captures the mirroring window as PNG.
func (c *Client) Screenshot(ctx context.Context) (Screen, error) {
	var raw struct {
		PNGBase64 string  `json:"png_base64"`
		ImgW      int     `json:"img_w"`
		ImgH      int     `json:"img_h"`
		Scale     float64 `json:"scale"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/screenshot", nil, &raw); err != nil {
		return Screen{}, err
	}
	png, err := base64.StdEncoding.DecodeString(raw.PNGBase64)
	if err != nil {
		return Screen{}, fmt.Errorf("mirrord: bad screenshot payload: %w", err)
	}
	return Screen{PNG: png, ImgW: raw.ImgW, ImgH: raw.ImgH, Scale: raw.Scale}, nil
}

// OCR captures the window and runs Vision text recognition over it.
// A zero-value opts reproduces the legacy call exactly (GET, no body).
func (c *Client) OCR(ctx context.Context, opts OCROptions) (OCRResult, error) {
	var raw struct {
		Boxes       []OCRBox `json:"boxes"`
		ImgW        int      `json:"img_w"`
		ImgH        int      `json:"img_h"`
		Scale       float64  `json:"scale"`
		ServedScale float64  `json:"served_scale"`
		Level       string   `json:"level"`
		Rect        *OCRRect `json:"rect"`
	}
	method := http.MethodGet
	var body any
	if !opts.zero() {
		method = http.MethodPost
		req := map[string]any{}
		if opts.Level != "" {
			req["level"] = opts.Level
		}
		if opts.Rect != nil {
			req["rect"] = opts.Rect
		}
		if opts.MaxDim > 0 {
			req["max_dim"] = opts.MaxDim
		}
		body = req
	}
	if err := c.call(ctx, method, "/v1/ocr", body, &raw); err != nil {
		return OCRResult{}, err
	}
	res := OCRResult{
		Boxes: raw.Boxes, ImgW: raw.ImgW, ImgH: raw.ImgH, Scale: raw.Scale,
		ServedScale: raw.ServedScale, Level: raw.Level, Rect: raw.Rect,
	}
	if res.ServedScale == 0 {
		res.ServedScale = 1
	}
	if res.Level == "" {
		res.Level = "accurate"
	}
	return res, nil
}
