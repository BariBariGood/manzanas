package client

import (
	"context"

	"github.com/BariBariGood/manzanas/proto"
)

// DevicesGet fetches the daemon's current runtime device configuration
// (GET /v0/devices).
func (c *Client) DevicesGet(ctx context.Context) (proto.DevicesConfig, error) {
	var cfg proto.DevicesConfig
	err := c.do(ctx, "GET", "/v0/devices", nil, &cfg)
	return cfg, err
}

// DevicesApply replaces the daemon's runtime device configuration
// (POST /v0/devices) and returns the applied config.
func (c *Client) DevicesApply(ctx context.Context, cfg proto.DevicesConfig) (proto.DevicesConfig, error) {
	var out proto.DevicesConfig
	err := c.do(ctx, "POST", "/v0/devices", cfg, &out)
	return out, err
}
