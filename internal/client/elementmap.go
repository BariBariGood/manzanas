package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/BariBariGood/manzanas/internal/elementmap"
)

// ElementMapList calls GET /v0/elementmap and returns the daemon's
// learned element-map entries.
func (c *Client) ElementMapList(ctx context.Context) ([]elementmap.Entry, error) {
	var out struct {
		Elements []elementmap.Entry `json:"elements"`
	}
	err := c.do(ctx, http.MethodGet, "/v0/elementmap", nil, &out)
	return out.Elements, err
}

// ElementMapRemove calls DELETE /v0/elementmap/{bundle_id}?name=... An
// empty deviceClass removes the name across all device classes. Returns
// how many entries were removed.
func (c *Client) ElementMapRemove(ctx context.Context, bundleID, name, deviceClass string) (int, error) {
	var out struct {
		Removed int `json:"removed"`
	}
	path := fmt.Sprintf("/v0/elementmap/%s?name=%s", url.PathEscape(bundleID), url.QueryEscape(name))
	if deviceClass != "" {
		path += "&device_class=" + url.QueryEscape(deviceClass)
	}
	err := c.do(ctx, http.MethodDelete, path, nil, &out)
	return out.Removed, err
}
