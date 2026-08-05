package client

import (
	"context"
	"net/http"

	"github.com/BariBariGood/manzanas/proto"
)

// OpenStream calls POST /v0/streams and returns the negotiated offer.
func (c *Client) OpenStream(ctx context.Context, req proto.StreamRequest) (proto.StreamOffer, error) {
	var offer proto.StreamOffer
	err := c.do(ctx, http.MethodPost, "/v0/streams", req, &offer)
	return offer, err
}
