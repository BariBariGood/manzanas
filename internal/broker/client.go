package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/BariBariGood/manzanas/proto"
)

// daemonClient is a minimal typed HTTP client for the daemon v0 protocol.
type daemonClient struct {
	http *http.Client
}

// daemonError carries a daemon's wire error plus its HTTP status so
// handlers can forward it verbatim.
type daemonError struct {
	Status int
	Err    proto.Error
}

func (e *daemonError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, e.Err.Code, e.Err.Message)
}

// token is the per-host bearer token; empty sends no Authorization.
func (c *daemonClient) getJSON(ctx context.Context, token, url string, out any) error {
	return c.do(ctx, token, http.MethodGet, url, nil, out)
}

func (c *daemonClient) postJSON(ctx context.Context, token, url string, in, out any) error {
	return c.do(ctx, token, http.MethodPost, url, in, out)
}

func (c *daemonClient) deleteJSON(ctx context.Context, token, url string, out any) error {
	return c.do(ctx, token, http.MethodDelete, url, nil, out)
}

// getJSONCode is getJSON returning the HTTP status code alongside, for
// callers where an absent endpoint (404/501 from an older daemon, in
// any body shape) is an expected, non-error outcome.
func (c *daemonClient) getJSONCode(ctx context.Context, token, url string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("GET %s: unexpected status %d", url, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("GET %s: decode response: %w", url, err)
		}
	}
	return resp.StatusCode, nil
}

func (c *daemonClient) do(ctx context.Context, token, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var we proto.Error
		if json.Unmarshal(data, &we) == nil && we.Code != "" {
			return &daemonError{Status: resp.StatusCode, Err: we}
		}
		return fmt.Errorf("%s %s: unexpected status %d", method, url, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decode response: %w", method, url, err)
		}
	}
	return nil
}
