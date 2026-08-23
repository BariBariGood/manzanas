package client

import (
	"context"
	"net/url"

	"github.com/BariBariGood/manzanas/proto"
)

// Doctor fetches the daemon's setup diagnosis (GET /v0/doctor). target,
// when non-empty, restricts per-device checks to that UDID. Older
// daemons answer 404 (or 501); callers surface that as "upgrade the
// daemon".
func (c *Client) Doctor(ctx context.Context, target string) (proto.DoctorReport, error) {
	path := "/v0/doctor"
	if target != "" {
		path += "?target=" + url.QueryEscape(target)
	}
	var r proto.DoctorReport
	err := c.do(ctx, "GET", path, nil, &r)
	return r, err
}
