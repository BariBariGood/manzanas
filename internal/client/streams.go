package client

import (
	"context"
	"net/http"

	"github.com/BariBariGood/manzanas/proto"
)

// OpenStream calls POST /v0/streams and returns the negotiated offer.
// Lease-identified requests route to the daemon owning the lease; the
// offer's relative URLs are then relative to AddrForLease(lease_id).
func (c *Client) OpenStream(ctx context.Context, req proto.StreamRequest) (proto.StreamOffer, error) {
	var offer proto.StreamOffer
	err := c.leaseDo(ctx, req.LeaseID, http.MethodPost, "/v0/streams", req, &offer)
	return offer, err
}
