// Package client is the thin Go client for the manzanasd v0 wire protocol.
// It maps 1:1 onto proto/PROTOCOL.md — no client-side magic.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// DefaultAddr is used when neither --daemon nor MANZANASD_ADDR is set.
const DefaultAddr = "http://127.0.0.1:7433"

// APIError is a non-2xx protocol error (proto.Error plus the HTTP status).
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ConnError wraps transport-level failures (daemon unreachable) with a
// friendly message including the address that was tried.
type ConnError struct {
	Addr string
	Err  error
}

func (e *ConnError) Error() string {
	return fmt.Sprintf("cannot reach manzanasd at %s (set --daemon or MANZANASD_ADDR): %v", e.Addr, e.Err)
}

func (e *ConnError) Unwrap() error { return e.Err }

// IsLeaseLost reports whether err means the lease is gone (expired,
// released, or unknown to the daemon).
func IsLeaseLost(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Code == proto.ErrLeaseExpired || ae.Code == proto.ErrNotFound
}

// Client speaks the manzanasd v0 HTTP protocol. Pointed at a
// manzanas-broker, it follows the host_addr annotation on leases so
// lease-scoped calls reach the owning daemon transparently (see route.go).
type Client struct {
	base  string
	token string
	http  *http.Client

	mu      sync.Mutex
	routes  map[string]string  // lease_id -> owning daemon base URL
	derived map[string]*Client // daemon base URL -> derived client
}

// New creates a Client for the daemon at addr ("host:port" or full URL).
func New(addr string) *Client {
	if addr == "" {
		addr = DefaultAddr
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	// No overall request timeout: long-running operations (install_app on a
	// large bundle, queued-lease waits) are bounded by the caller's ctx.
	// Connection establishment still fails fast.
	return &Client{
		base: strings.TrimRight(addr, "/"),
		http: &http.Client{Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		}},
	}
}

// Addr returns the base URL the client talks to.
func (c *Client) Addr() string { return c.base }

// SetToken sets the bearer token sent as Authorization on every request,
// for daemons/brokers running with --auth-token. Empty disables it. The
// token propagates to the per-host clients derived for broker
// transparency, so a change applies to every daemon this client talks to.
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	c.token = token
	derived := make([]*Client, 0, len(c.derived))
	for _, d := range c.derived {
		derived = append(derived, d)
	}
	c.mu.Unlock()
	for _, d := range derived {
		d.SetToken(token)
	}
}

// Token returns the configured bearer token (empty when auth is off).
func (c *Client) Token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	var ctype string
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
		ctype = "application/json"
	}
	return c.doReader(ctx, method, path, ctype, rdr, out)
}

// doReader is do with a raw request body (e.g. artifact bytes) instead of
// a JSON-marshalled one.
func (c *Client) doReader(ctx context.Context, method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if t := c.Token(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			return &ConnError{Addr: c.base, Err: ue.Err}
		}
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var pe proto.Error
		if json.Unmarshal(data, &pe) == nil && pe.Code != "" {
			return &APIError{Status: resp.StatusCode, Code: pe.Code, Message: pe.Message}
		}
		return &APIError{Status: resp.StatusCode, Code: proto.ErrInternal,
			Message: strings.TrimSpace(string(data))}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Health calls GET /v0/healthz.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/v0/healthz", nil, nil)
}
