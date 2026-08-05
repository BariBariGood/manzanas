package client

import (
	"context"
	"strconv"

	"github.com/BariBariGood/manzanas/internal/broker"
)

// FleetHosts lists a broker's hosts with health (GET /v0/fleet/hosts).
func (c *Client) FleetHosts(ctx context.Context) ([]broker.HostHealth, error) {
	var resp struct {
		Hosts []broker.HostHealth `json:"hosts"`
	}
	if err := c.do(ctx, "GET", "/v0/fleet/hosts", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Hosts, nil
}

// FleetPlacements fetches a broker's recent placement decisions, newest
// first (GET /v0/fleet/placements); n <= 0 fetches all retained.
func (c *Client) FleetPlacements(ctx context.Context, n int) ([]broker.PlacementDecision, error) {
	path := "/v0/fleet/placements"
	if n > 0 {
		path += "?n=" + strconv.Itoa(n)
	}
	var resp struct {
		Placements []broker.PlacementDecision `json:"placements"`
	}
	if err := c.do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Placements, nil
}

// FleetHints fetches a broker's current warm-pool rebalancing hints
// (GET /v0/fleet/hints).
func (c *Client) FleetHints(ctx context.Context) (int, []broker.HostHints, error) {
	var resp struct {
		WindowSeconds int                `json:"window_seconds"`
		Hosts         []broker.HostHints `json:"hosts"`
	}
	if err := c.do(ctx, "GET", "/v0/fleet/hints", nil, &resp); err != nil {
		return 0, nil, err
	}
	return resp.WindowSeconds, resp.Hosts, nil
}
