package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/BariBariGood/manzanas/proto"
)

// ListTargets calls GET /v0/targets.
func (c *Client) ListTargets(ctx context.Context) ([]proto.Target, error) {
	var out struct {
		Targets []proto.Target `json:"targets"`
	}
	err := c.do(ctx, http.MethodGet, "/v0/targets", nil, &out)
	return out.Targets, err
}

// BootTarget calls POST /v0/targets/{udid}/boot.
func (c *Client) BootTarget(ctx context.Context, udid, leaseID string) (proto.Target, error) {
	var t proto.Target
	err := c.do(ctx, http.MethodPost, "/v0/targets/"+url.PathEscape(udid)+"/boot",
		map[string]string{"lease_id": leaseID}, &t)
	return t, err
}

// BootTargetWait calls POST /v0/targets/{udid}/boot?wait=true: on a
// gate-refused boot the daemon retries server-side until the boot is
// accepted or its wait budget runs out, so the caller need not busy-poll.
func (c *Client) BootTargetWait(ctx context.Context, udid, leaseID string) (proto.Target, error) {
	var t proto.Target
	err := c.do(ctx, http.MethodPost, "/v0/targets/"+url.PathEscape(udid)+"/boot?wait=true",
		map[string]string{"lease_id": leaseID}, &t)
	return t, err
}

// ShutdownTarget calls POST /v0/targets/{udid}/shutdown.
func (c *Client) ShutdownTarget(ctx context.Context, udid, leaseID string) (proto.Target, error) {
	var t proto.Target
	err := c.do(ctx, http.MethodPost, "/v0/targets/"+url.PathEscape(udid)+"/shutdown",
		map[string]string{"lease_id": leaseID}, &t)
	return t, err
}
