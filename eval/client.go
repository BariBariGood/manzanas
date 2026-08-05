package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// Client is a minimal HTTP protocol client for the eval harness. It speaks
// the v0 REST surface using the wire types from proto/. It is intentionally
// self-contained so the harness stays decoupled from daemon internals; if a
// shared client package lands, this can be swapped for it.
type Client struct {
	baseURL string
	http    *http.Client

	// OverloadBudget bounds retrying boot requests the daemon refuses
	// with 503 overloaded (PROTOCOL.md §2 host safety gates). 0 disables
	// retrying: the first 503 is returned to the caller.
	OverloadBudget time.Duration

	// sleep is swapped in tests to avoid real waiting.
	sleep func(ctx context.Context, d time.Duration) error
}

// NewClient returns a client for a daemon at baseURL (e.g.
// "http://mac-host:7433").
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{},
	}
}

// APIError is a non-2xx protocol response.
type APIError struct {
	Status int
	Err    proto.Error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("daemon error %d %s: %s", e.Status, e.Err.Code, e.Err.Message)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var pe proto.Error
		if err := json.Unmarshal(raw, &pe); err != nil || pe.Code == "" {
			pe = proto.Error{Code: proto.ErrInternal, Message: strings.TrimSpace(string(raw))}
		}
		return &APIError{Status: resp.StatusCode, Err: pe}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decoding %s %s response: %w", method, path, err)
		}
	}
	return nil
}

// Healthz checks the daemon is up.
func (c *Client) Healthz(ctx context.Context) error {
	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.do(ctx, http.MethodGet, "/v0/healthz", nil, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("daemon healthz not ok")
	}
	return nil
}

// Targets lists all targets.
func (c *Client) Targets(ctx context.Context) ([]proto.Target, error) {
	var out struct {
		Targets []proto.Target `json:"targets"`
	}
	if err := c.do(ctx, http.MethodGet, "/v0/targets", nil, &out); err != nil {
		return nil, err
	}
	return out.Targets, nil
}

// Target returns the target with the given UDID.
func (c *Client) Target(ctx context.Context, udid string) (*proto.Target, error) {
	targets, err := c.Targets(ctx)
	if err != nil {
		return nil, err
	}
	for i := range targets {
		if targets[i].UDID == udid {
			return &targets[i], nil
		}
	}
	return nil, fmt.Errorf("target %s not in daemon target list", udid)
}

// AcquireLease requests a lease and, if queued, polls until it is active or
// ctx expires.
func (c *Client) AcquireLease(ctx context.Context, req proto.AcquireLeaseRequest) (*proto.Lease, error) {
	var lease proto.Lease
	if err := c.do(ctx, http.MethodPost, "/v0/leases", req, &lease); err != nil {
		return nil, err
	}
	// Best effort: don't leave an abandoned lease behind on any
	// non-active exit (ctx expiry, transient poll error, unexpected state).
	acquired := false
	defer func() {
		if acquired {
			return
		}
		relCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _ = c.ReleaseLease(relCtx, lease.ID)
		cancel()
	}()
	for lease.State == proto.LeaseQueued {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for queued lease %s: %w", lease.ID, ctx.Err())
		case <-time.After(2 * time.Second):
		}
		if err := c.do(ctx, http.MethodGet, "/v0/leases/"+url.PathEscape(lease.ID), nil, &lease); err != nil {
			return nil, err
		}
	}
	if lease.State != proto.LeaseActive {
		return nil, fmt.Errorf("lease %s in unexpected state %q", lease.ID, lease.State)
	}
	acquired = true
	return &lease, nil
}

// RenewLease extends an active lease's TTL. ttlSeconds <= 0 keeps the
// original TTL.
func (c *Client) RenewLease(ctx context.Context, id string, ttlSeconds int) (*proto.Lease, error) {
	var lease proto.Lease
	req := proto.RenewLeaseRequest{TTLSeconds: ttlSeconds}
	if err := c.do(ctx, http.MethodPost, "/v0/leases/"+url.PathEscape(id)+"/renew", req, &lease); err != nil {
		return nil, err
	}
	return &lease, nil
}

// ReleaseLease releases a lease (idempotent).
func (c *Client) ReleaseLease(ctx context.Context, id string) (*proto.Lease, error) {
	var lease proto.Lease
	if err := c.do(ctx, http.MethodDelete, "/v0/leases/"+url.PathEscape(id), nil, &lease); err != nil {
		return nil, err
	}
	return &lease, nil
}

// Boot boots the leased target and polls until state is Booted or ctx
// expires. When the daemon's host safety gates refuse the boot with
// 503 overloaded (see PROTOCOL.md §2), the request is retried with
// exponential backoff and jitter until OverloadBudget is exhausted.
func (c *Client) Boot(ctx context.Context, udid, leaseID string) error {
	body := map[string]string{"lease_id": leaseID}
	path := "/v0/targets/" + url.PathEscape(udid) + "/boot"
	deadline := time.Now().Add(c.OverloadBudget)
	backoff := overloadBackoffBase
	for {
		err := c.do(ctx, http.MethodPost, path, body, nil)
		if err == nil {
			return c.waitForState(ctx, udid, proto.StateBooted)
		}
		if !IsOverloaded(err) || c.OverloadBudget <= 0 {
			return err
		}
		// Jitter the backoff (uniform in [0.5x, 1.5x)) so concurrent
		// harnesses don't retry in lockstep. The last sleep is clamped to
		// whatever budget remains so it is spent in full.
		sleep := time.Duration(rand.Int63n(int64(backoff)) + int64(backoff)/2)
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("boot %s: overload retry budget %s exhausted: %w", udid, c.OverloadBudget, err)
		}
		if sleep > remaining {
			sleep = remaining
		}
		if err := c.doSleep(ctx, sleep); err != nil {
			return fmt.Errorf("boot %s: waiting to retry after overloaded: %w", udid, err)
		}
		backoff *= 2
		if backoff > overloadBackoffCap {
			backoff = overloadBackoffCap
		}
	}
}

const (
	overloadBackoffBase = 2 * time.Second
	overloadBackoffCap  = 30 * time.Second
)

// IsOverloaded reports whether err is a daemon 503 overloaded refusal
// (warm-pool gate or dispatch queue pushback).
func IsOverloaded(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == http.StatusServiceUnavailable && ae.Err.Code == proto.ErrOverloaded
}

func (c *Client) doSleep(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// Shutdown shuts the leased target down and polls until state is Shutdown.
func (c *Client) Shutdown(ctx context.Context, udid, leaseID string) error {
	body := map[string]string{"lease_id": leaseID}
	if err := c.do(ctx, http.MethodPost, "/v0/targets/"+url.PathEscape(udid)+"/shutdown", body, nil); err != nil {
		return err
	}
	return c.waitForState(ctx, udid, proto.StateShutdown)
}

func (c *Client) waitForState(ctx context.Context, udid string, want proto.TargetState) error {
	for {
		t, err := c.Target(ctx, udid)
		if err != nil {
			return err
		}
		if t.State == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for target %s to reach %s (currently %s): %w", udid, want, t.State, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// Action dispatches an action for the leased target.
func (c *Client) Action(ctx context.Context, req proto.ActionRequest) (*proto.ActionResult, error) {
	var res proto.ActionResult
	if err := c.do(ctx, http.MethodPost, "/v0/actions", req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Fixture applies a named state fixture.
func (c *Client) Fixture(ctx context.Context, req proto.FixtureRequest) error {
	return c.do(ctx, http.MethodPost, "/v0/state/fixtures", req, nil)
}

// Snapshot captures a snapshot of the leased (shutdown) target.
func (c *Client) Snapshot(ctx context.Context, req proto.SnapshotRequest) (*proto.SnapshotInfo, error) {
	var info proto.SnapshotInfo
	if err := c.do(ctx, http.MethodPost, "/v0/state/snapshots", req, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ListSnapshots returns the snapshots visible to the lease.
func (c *Client) ListSnapshots(ctx context.Context, leaseID string) ([]proto.SnapshotInfo, error) {
	var out struct {
		Snapshots []proto.SnapshotInfo `json:"snapshots"`
	}
	path := "/v0/state/snapshots?lease_id=" + url.QueryEscape(leaseID)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Snapshots, nil
}

// DeleteSnapshot removes a snapshot taken from the lease's target.
func (c *Client) DeleteSnapshot(ctx context.Context, id, leaseID string) error {
	path := "/v0/state/snapshots/" + url.PathEscape(id) + "?lease_id=" + url.QueryEscape(leaseID)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// Restore restores the leased target to a snapshot.
func (c *Client) Restore(ctx context.Context, req proto.RestoreRequest) (*proto.RestoreResult, error) {
	var res proto.RestoreResult
	if err := c.do(ctx, http.MethodPost, "/v0/state/restore", req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Erase factory-resets the leased (shutdown) target.
func (c *Client) Erase(ctx context.Context, req proto.EraseRequest) error {
	return c.do(ctx, http.MethodPost, "/v0/state/erase", req, nil)
}
