package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// AcquireLease calls POST /v0/leases. The returned lease may be queued.
func (c *Client) AcquireLease(ctx context.Context, req proto.AcquireLeaseRequest) (proto.Lease, error) {
	var l proto.Lease
	err := c.do(ctx, http.MethodPost, "/v0/leases", req, &l)
	if err == nil {
		c.noteLease(l)
	}
	return l, err
}

// GetLease calls GET /v0/leases/{id}: through a broker when there is one,
// falling back to the owning daemon if the broker is down.
func (c *Client) GetLease(ctx context.Context, id string) (proto.Lease, error) {
	var l proto.Lease
	err := c.leaseControlDo(ctx, id, http.MethodGet, "/v0/leases/"+url.PathEscape(id), nil, &l)
	return l, err
}

// RenewLease calls POST /v0/leases/{id}/renew: through a broker when
// there is one, falling back to the owning daemon if the broker is down.
func (c *Client) RenewLease(ctx context.Context, id string, ttlSeconds int) (proto.Lease, error) {
	var l proto.Lease
	err := c.leaseControlDo(ctx, id, http.MethodPost, "/v0/leases/"+url.PathEscape(id)+"/renew",
		proto.RenewLeaseRequest{TTLSeconds: ttlSeconds}, &l)
	return l, err
}

// ReleaseLease calls DELETE /v0/leases/{id}: through a broker when there
// is one, falling back to the owning daemon if the broker is down.
func (c *Client) ReleaseLease(ctx context.Context, id string) (proto.Lease, error) {
	var l proto.Lease
	err := c.leaseControlDo(ctx, id, http.MethodDelete, "/v0/leases/"+url.PathEscape(id), nil, &l)
	return l, err
}

// ListLeases calls GET /v0/leases.
func (c *Client) ListLeases(ctx context.Context) ([]proto.Lease, error) {
	var out struct {
		Leases []proto.Lease `json:"leases"`
	}
	err := c.do(ctx, http.MethodGet, "/v0/leases", nil, &out)
	for _, l := range out.Leases {
		c.noteLease(l)
	}
	return out.Leases, err
}

// isTransient reports whether err is likely momentary (network blip or
// daemon-side 5xx) and worth retrying while waiting on a queued lease.
func isTransient(err error) bool {
	var ce *ConnError
	if errors.As(err, &ce) {
		return true
	}
	var ae *APIError
	return errors.As(err, &ae) && ae.Status >= 500
}

// WaitForLease polls GET /v0/leases/{id} until the lease becomes active,
// calling onQueued (if non-nil) with each queued snapshot. It returns an
// error if the lease reaches a terminal state or ctx is done.
func (c *Client) WaitForLease(ctx context.Context, id string, poll time.Duration,
	onQueued func(proto.Lease)) (proto.Lease, error) {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	const maxTransientFailures = 5
	transient := 0
	for {
		l, err := c.GetLease(ctx, id)
		if err != nil {
			// Tolerate brief network blips / 5xx so a queued lease isn't
			// abandoned server-side; give up after several in a row.
			if isTransient(err) && transient < maxTransientFailures {
				transient++
				select {
				case <-ctx.Done():
					return proto.Lease{}, ctx.Err()
				case <-t.C:
				}
				continue
			}
			return proto.Lease{}, err
		}
		transient = 0
		switch l.State {
		case proto.LeaseActive:
			return l, nil
		case proto.LeaseQueued:
			if onQueued != nil {
				onQueued(l)
			}
		default:
			return l, &APIError{Status: http.StatusGone, Code: proto.ErrLeaseExpired,
				Message: "lease became " + string(l.State) + " while waiting"}
		}
		select {
		case <-ctx.Done():
			return proto.Lease{}, ctx.Err()
		case <-t.C:
		}
	}
}
