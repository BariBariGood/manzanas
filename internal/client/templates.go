package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/BariBariGood/manzanas/internal/templates"
)

// TemplatesList calls GET /v0/templates and returns the daemon's template
// library entries.
func (c *Client) TemplatesList(ctx context.Context) ([]templates.Entry, error) {
	var out struct {
		Templates []templates.Entry `json:"templates"`
	}
	err := c.do(ctx, http.MethodGet, "/v0/templates", nil, &out)
	return out.Templates, err
}

// TemplatesRemove calls DELETE /v0/templates/{bundle_id}/{name}. An empty
// deviceClass removes the name across all device classes. Returns how many
// templates were removed.
func (c *Client) TemplatesRemove(ctx context.Context, bundleID, name, deviceClass string) (int, error) {
	var out struct {
		Removed int `json:"removed"`
	}
	path := fmt.Sprintf("/v0/templates/%s/%s", url.PathEscape(bundleID), url.PathEscape(name))
	if deviceClass != "" {
		path += "?device_class=" + url.QueryEscape(deviceClass)
	}
	err := c.do(ctx, http.MethodDelete, path, nil, &out)
	return out.Removed, err
}
