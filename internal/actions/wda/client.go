// Package wda is a minimal pure-Go client for WebDriverAgent's HTTP
// protocol (github.com/appium/WebDriverAgent) — the on-device XCUITest
// agent that provides HID input, screenshots, and the element tree for
// physical devices. No Appium server is involved: manzanasd speaks WDA's
// REST endpoints directly, matching the thin-resident-helper pattern the
// simulator warm path uses with simbridge.
package wda

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout bounds each WDA HTTP round-trip.
const DefaultTimeout = 30 * time.Second

// Client talks to one WDA instance (one device). It lazily creates a WDA
// session on first use and re-creates it once when the agent reports the
// session invalid (WDA drops sessions when the runner restarts).
type Client struct {
	base  string
	httpc *http.Client

	mu        sync.Mutex
	sessionID string
}

// New returns a Client for a WDA base URL, e.g. "http://127.0.0.1:8100".
func New(baseURL string) *Client {
	return &Client{
		base:  strings.TrimRight(baseURL, "/"),
		httpc: &http.Client{Timeout: DefaultTimeout},
	}
}

// BaseURL returns the configured WDA endpoint.
func (c *Client) BaseURL() string { return c.base }

// wdaResponse is the WebDriver wire envelope: value carries the payload
// (or an error object), sessionId rides along on session-scoped calls.
type wdaResponse struct {
	Value     json.RawMessage `json:"value"`
	SessionID string          `json:"sessionId"`
}

// wdaError is the error shape inside a failed response's value.
type wdaError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Error is a WDA protocol failure.
type Error struct {
	Status  int    // HTTP status
	Kind    string // WDA error kind, e.g. "invalid session id"
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("wda: %s: %s (http %d)", e.Kind, e.Message, e.Status)
}

// do issues one request and decodes the WebDriver envelope.
func (c *Client) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wda: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("wda: read %s %s: %w", method, path, err)
	}
	var env wdaResponse
	decErr := json.Unmarshal(data, &env)
	// Classify by HTTP status before insisting on a JSON envelope: an
	// error reply may be plain text (a proxy, a restarted runner), and it
	// must still surface as a typed *Error so session recovery can fire.
	if resp.StatusCode/100 != 2 {
		var we wdaError
		if decErr == nil {
			_ = json.Unmarshal(env.Value, &we)
		}
		if we.Error == "" {
			we.Error = "unknown error"
			we.Message = strings.TrimSpace(string(data))
		}
		return nil, &Error{Status: resp.StatusCode, Kind: we.Error, Message: we.Message}
	}
	if decErr != nil {
		return nil, fmt.Errorf("wda: decode %s %s: %w", method, path, decErr)
	}
	return env.Value, nil
}

// Status probes GET /status (no session required).
func (c *Client) Status(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/status", nil)
	return err
}

// Session returns the current WDA session ID, creating one if needed
// (POST /session with empty capabilities attaches to whatever app is
// frontmost).
func (c *Client) Session(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionID != "" {
		return c.sessionID, nil
	}
	val, err := c.do(ctx, http.MethodPost, "/session",
		map[string]any{"capabilities": map[string]any{"alwaysMatch": map[string]any{}}})
	if err != nil {
		return "", err
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(val, &created); err != nil || created.SessionID == "" {
		return "", fmt.Errorf("wda: session create returned no sessionId: %s", string(val))
	}
	c.sessionID = created.SessionID
	return c.sessionID, nil
}

// invalidateSession drops the cached session so the next call re-creates it.
func (c *Client) invalidateSession() {
	c.mu.Lock()
	c.sessionID = ""
	c.mu.Unlock()
}

// isInvalidSession recognizes a dropped/stale WDA session.
func isInvalidSession(err error) bool {
	var we *Error
	return errors.As(err, &we) &&
		(strings.Contains(we.Kind, "invalid session") ||
			strings.Contains(we.Message, "invalid session"))
}

// inSession runs one session-scoped call, re-creating the session and
// retrying once when WDA reports it invalid.
func (c *Client) inSession(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		sid, err := c.Session(ctx)
		if err != nil {
			return nil, err
		}
		val, err := c.do(ctx, method, "/session/"+sid+path, body)
		if err != nil && isInvalidSession(err) && attempt == 0 {
			c.invalidateSession()
			continue
		}
		return val, err
	}
}

// Tap taps at screen coordinates (points).
func (c *Client) Tap(ctx context.Context, x, y float64) error {
	_, err := c.inSession(ctx, http.MethodPost, "/wda/tap", map[string]any{"x": x, "y": y})
	return err
}

// Keys types text into the focused element (POST /wda/keys).
func (c *Client) Keys(ctx context.Context, text string) error {
	_, err := c.inSession(ctx, http.MethodPost, "/wda/keys",
		map[string]any{"value": strings.Split(text, "")})
	return err
}

// Screenshot captures the screen as PNG bytes (GET /screenshot,
// session-less).
func (c *Client) Screenshot(ctx context.Context) ([]byte, error) {
	val, err := c.do(ctx, http.MethodGet, "/screenshot", nil)
	if err != nil {
		return nil, err
	}
	var b64 string
	if err := json.Unmarshal(val, &b64); err != nil {
		return nil, fmt.Errorf("wda: screenshot value is not a string: %w", err)
	}
	png, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("wda: screenshot base64: %w", err)
	}
	return png, nil
}

// Source returns the element tree as XML (GET /source, session-less).
func (c *Client) Source(ctx context.Context) (string, error) {
	val, err := c.do(ctx, http.MethodGet, "/source", nil)
	if err != nil {
		return "", err
	}
	var xml string
	if err := json.Unmarshal(val, &xml); err != nil {
		return "", fmt.Errorf("wda: source value is not a string: %w", err)
	}
	return xml, nil
}
