package client

import (
	"context"
	"net/url"

	"github.com/BariBariGood/manzanas/proto"
)

// StartRun submits a run (POST /v0/runs). With req.Async=false the call
// blocks until the run finishes (bounded by the spec's run timeout
// server-side); with Async=true it returns the pending run immediately.
func (c *Client) StartRun(ctx context.Context, req proto.RunRequest) (proto.Run, error) {
	var run proto.Run
	err := c.do(ctx, "POST", "/v0/runs", req, &run)
	return run, err
}

// GetRun fetches one run resource (GET /v0/runs/{id}).
func (c *Client) GetRun(ctx context.Context, id string) (proto.Run, error) {
	var run proto.Run
	err := c.do(ctx, "GET", "/v0/runs/"+url.PathEscape(id), nil, &run)
	return run, err
}

// ListRuns lists retained runs, newest first (GET /v0/runs).
func (c *Client) ListRuns(ctx context.Context) ([]proto.Run, error) {
	var out proto.RunList
	err := c.do(ctx, "GET", "/v0/runs", nil, &out)
	return out.Runs, err
}
