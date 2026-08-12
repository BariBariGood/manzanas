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

// defaultReadyWait bounds how long the first action after an observed
// runner death waits for GET /status before proceeding, so an unrelated
// action doesn't inherit the previous action's crash while the
// supervisor relaunches the runner.
const defaultReadyWait = 15 * time.Second

// sessionCaps are the capabilities every session is created with.
// Quiescence/idle waiting must be off: XCTest's idle wait during
// snapshot generation kills the runner outright on large apps (an
// animating TikTok feed never goes idle). These are sent unprefixed —
// real-world WDA builds ignore the "appium:"-prefixed forms.
func sessionCaps() map[string]any {
	return map[string]any{
		"waitForQuiescence":         false,
		"shouldWaitForQuiescence":   false,
		"waitForIdleTimeout":        0,
		"shouldUseCompactResponses": true,
		"useFirstMatch":             true,
	}
}

// Client talks to one WDA instance (one device). It lazily creates a WDA
// session on first use and re-creates it once when the agent reports the
// session invalid (WDA drops sessions when the runner restarts).
type Client struct {
	base  string
	httpc *http.Client

	// createMu serializes session creation; mu guards the fields below
	// and is never held across an HTTP round-trip.
	createMu  sync.Mutex
	mu        sync.Mutex
	sessionID string
	// needsReady is set when a request dies at the transport level (the
	// runner crashed mid-action); the next request first waits for
	// GET /status readiness instead of hitting a relaunching runner.
	needsReady bool
	// readyExhausted is set when a readiness wait ran out its whole
	// budget without /status answering: the runner is down, not
	// relaunching, so transport failures stop re-arming the wait (and
	// actions fail fast) until a request reaches the runner again.
	readyExhausted bool
	// readyBy is the wall-clock end of the current readiness wait, armed
	// once when needsReady is set. Callers whose own contexts are shorter
	// than the ready budget each wait toward the same absolute deadline,
	// so exhaustion is reached (and actions start failing fast) even when
	// no single caller lives the full budget.
	readyBy   time.Time
	readyWait time.Duration
}

// New returns a Client for a WDA base URL, e.g. "http://127.0.0.1:8100".
func New(baseURL string) *Client {
	return &Client{
		base:      strings.TrimRight(baseURL, "/"),
		httpc:     &http.Client{Timeout: DefaultTimeout},
		readyWait: defaultReadyWait,
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

// raw issues one request, handling transport-level death of the runner
// (mark the client not-ready, drop the session, surface a retryable
// message) but leaving the body uninterpreted.
func (c *Client) raw(ctx context.Context, method, path string, body any) (*http.Response, []byte, error) {
	c.awaitReady(ctx)
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, nil, c.transportErr(ctx, method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, nil, c.transportErr(ctx, method, path, err)
	}
	c.mu.Lock()
	c.readyExhausted = false
	c.mu.Unlock()
	return resp, data, nil
}

// transportErr records a transport-level failure (connection reset, EOF:
// the runner died mid-action) so the next request waits for readiness,
// and replaces the bare network error with a retryable message. Context
// cancellation is the caller's doing, and a client-side round-trip
// timeout means the runner is slow, not dead — neither counts as a
// restart.
func (c *Client) transportErr(ctx context.Context, method, path string, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("wda: %s %s: %w", method, path, err)
	}
	c.mu.Lock()
	// After an exhausted readiness wait the runner is known-down: keep
	// failing fast instead of re-arming a 15s wait on every action.
	if !c.readyExhausted && !c.needsReady {
		c.needsReady = true
		c.readyBy = time.Now().Add(c.readyWait)
	}
	c.sessionID = ""
	c.mu.Unlock()
	return fmt.Errorf("wda: %s %s: WDA runner restarted, retry (%v)", method, path, err)
}

// awaitReady blocks the first request after an observed runner death
// until GET /status answers (or the ready budget runs out), so the
// supervisor's relaunch window doesn't fail unrelated actions.
func (c *Client) awaitReady(ctx context.Context) {
	c.mu.Lock()
	need := c.needsReady
	deadline := c.readyBy
	c.mu.Unlock()
	if !need {
		return
	}
	// Bound the whole wait (including a hung /status probe) by the ready
	// budget, not the client's per-request timeout.
	wctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(wctx, http.MethodGet, c.base+"/status", nil)
		if err != nil {
			return
		}
		if resp, err := c.httpc.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				c.mu.Lock()
				c.needsReady = false
				c.readyExhausted = false
				c.mu.Unlock()
				return
			}
		}
		if time.Now().After(deadline) {
			// The runner did not come back within the budget: disarm and
			// remember the exhaustion so follow-up transport failures
			// fail fast instead of re-arming the same wait; a request
			// that reaches the runner again resets this.
			c.mu.Lock()
			c.needsReady = false
			c.readyExhausted = true
			c.mu.Unlock()
			return
		}
		select {
		case <-wctx.Done():
			// Budget expiry counts as exhaustion; a caller cancellation
			// before the budget's end does not — needsReady stays armed
			// and the next caller resumes waiting toward the same readyBy.
			if time.Now().After(deadline) {
				c.mu.Lock()
				c.needsReady = false
				c.readyExhausted = true
				c.mu.Unlock()
			}
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// do issues one request and decodes the WebDriver envelope.
func (c *Client) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	resp, data, err := c.raw(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	return parseEnvelope(method, path, resp.StatusCode, data)
}

// parseEnvelope decodes the WebDriver envelope out of a response body.
func parseEnvelope(method, path string, status int, data []byte) (json.RawMessage, error) {
	var env wdaResponse
	decErr := json.Unmarshal(data, &env)
	// Classify by HTTP status before insisting on a JSON envelope: an
	// error reply may be plain text (a proxy, a restarted runner), and it
	// must still surface as a typed *Error so session recovery can fire.
	if status/100 != 2 {
		var we wdaError
		if decErr == nil {
			_ = json.Unmarshal(env.Value, &we)
		}
		if we.Error == "" {
			we.Error = "unknown error"
			we.Message = strings.TrimSpace(string(data))
		}
		return nil, &Error{Status: status, Kind: we.Error, Message: we.Message}
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

// Session returns the current WDA session ID, creating one if needed.
// Creation attaches to whatever app is frontmost and always applies
// sessionCaps (as W3C alwaysMatch and legacy desiredCapabilities, so
// every WDA flavour honors them); because re-creation goes through
// here too, the caps are re-applied after a runner restart drops the
// session.
func (c *Client) Session(ctx context.Context) (string, error) {
	c.createMu.Lock()
	defer c.createMu.Unlock()
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid != "" {
		return sid, nil
	}
	caps := sessionCaps()
	val, err := c.do(ctx, http.MethodPost, "/session", map[string]any{
		"capabilities":        map[string]any{"alwaysMatch": caps},
		"desiredCapabilities": caps,
	})
	if err != nil {
		return "", err
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(val, &created); err != nil || created.SessionID == "" {
		return "", fmt.Errorf("wda: session create returned no sessionId: %s", string(val))
	}
	c.mu.Lock()
	c.sessionID = created.SessionID
	c.mu.Unlock()
	return created.SessionID, nil
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

// pngMagic is the PNG file signature.
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// Screenshot captures the screen as PNG bytes (GET /screenshot,
// session-less). WDA builds disagree on the response shape — upstream
// answers the JSON envelope {"value":"<base64>"}, real-world builds
// answer raw image bytes — so the body is sniffed rather than assumed.
func (c *Client) Screenshot(ctx context.Context) ([]byte, error) {
	resp, data, err := c.raw(ctx, http.MethodGet, "/screenshot", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 &&
		(bytes.HasPrefix(data, pngMagic) ||
			strings.HasPrefix(resp.Header.Get("Content-Type"), "image/")) {
		return data, nil
	}
	val, err := parseEnvelope(http.MethodGet, "/screenshot", resp.StatusCode, data)
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

// Source returns the element tree as XML. It goes through the session
// route (GET /session/{id}/source) so the snapshot honors the session's
// capabilities — the session-less route ignores them, and its default
// quiescence wait crashes the runner on large apps.
func (c *Client) Source(ctx context.Context) (string, error) {
	val, err := c.inSession(ctx, http.MethodGet, "/source", nil)
	if err != nil {
		return "", err
	}
	var xml string
	if err := json.Unmarshal(val, &xml); err != nil {
		return "", fmt.Errorf("wda: source value is not a string: %w", err)
	}
	return xml, nil
}
