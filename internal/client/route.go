package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/BariBariGood/manzanas/proto"
)

// Broker transparency: a lease acquired through a manzanas-broker is
// annotated with host_addr — the daemon that owns it. The client caches
// lease_id → host_addr from every lease it sees and routes lease-scoped
// calls (actions, boot/shutdown, state, streams, recording, journal —
// run_id equals lease_id) to the owning daemon automatically, so CLI and
// MCP work pointed at a broker without re-pointing. Leases without a
// host_addr (single daemon) keep everything on the base endpoint,
// byte-identical to before.

// noteLease caches a broker-annotated lease's owning daemon address.
func (c *Client) noteLease(l proto.Lease) {
	if l.ID == "" || l.HostAddr == "" {
		return
	}
	c.noteRoute(l.ID, l.HostAddr)
}

func (c *Client) noteRoute(leaseID, hostAddr string) {
	addr := normalizeAddr(hostAddr)
	if addr == c.base {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.routes == nil {
		c.routes = make(map[string]string)
	}
	c.routes[leaseID] = addr
}

func normalizeAddr(addr string) string {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return strings.TrimRight(addr, "/")
}

// hostClient returns a Client for addr sharing this client's transport
// and route table.
func (c *Client) hostClient(addr string) *Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.derived == nil {
		c.derived = make(map[string]*Client)
	}
	if d, ok := c.derived[addr]; ok {
		return d
	}
	d := &Client{base: addr, token: c.token, http: c.http}
	c.derived[addr] = d
	return d
}

// forLease returns the client for a lease-scoped call: the owning daemon
// when a host_addr is cached for the lease, otherwise this client.
func (c *Client) forLease(leaseID string) *Client {
	c.mu.Lock()
	addr, ok := c.routes[leaseID]
	c.mu.Unlock()
	if !ok {
		return c
	}
	return c.hostClient(addr)
}

// AddrForLease returns the base URL lease-scoped calls for leaseID go to:
// the owning daemon's host_addr when known (broker-acquired lease),
// otherwise Addr(). Callers that absolutize server-relative URLs (e.g.
// stream offers) must use it instead of Addr().
func (c *Client) AddrForLease(leaseID string) string {
	if leaseID == "" {
		return c.base
	}
	return c.forLease(leaseID).base
}

// isRouteMiss reports whether err is the endpoint answering that it does
// not serve the route at all — the signature of a lease-scoped call
// hitting a broker, which serves only placement endpoints.
func isRouteMiss(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
		return false
	}
	// A broker envelopes mux 404s as {"code":"not_found","message":
	// "unknown route"}; a bare mux answers text/plain (surfaced without a
	// proto code). A JSON not_found with any other message is a real
	// daemon saying the lease/target/run is unknown.
	return (ae.Code == proto.ErrNotFound && ae.Message == "unknown route") ||
		ae.Code == proto.ErrInternal
}

// resolveLease fetches the lease from the base endpoint (a broker proxies
// GET by lease ID to the owning daemon) and caches its host_addr.
func (c *Client) resolveLease(ctx context.Context, leaseID string) *Client {
	var l proto.Lease
	if err := c.do(ctx, http.MethodGet, "/v0/leases/"+url.PathEscape(leaseID), nil, &l); err == nil {
		c.noteLease(l)
	}
	return c.forLease(leaseID)
}

// leaseDo routes a lease-scoped request to the daemon that owns the
// lease. Cached routes go direct; unknown leases try the base endpoint
// first and, on a broker route miss, resolve host_addr via the lease and
// retry — so a fresh process (each CLI invocation) recovers on its first
// call while the single-daemon path stays free of extra round trips.
func (c *Client) leaseDo(ctx context.Context, leaseID, method, path string, body, out any) error {
	lc := c.forLease(leaseID)
	err := lc.do(ctx, method, path, body, out)
	if lc == c && leaseID != "" && isRouteMiss(err) {
		if rc := c.resolveLease(ctx, leaseID); rc != c {
			return rc.do(ctx, method, path, body, out)
		}
		// The broker no longer knows the lease (terminal leases are
		// dropped from its routing table), so ask it for its host list
		// and try each daemon — this is how a fresh process reads the
		// journal of a finished run through a broker.
		if ok, err2 := c.fanOutDo(ctx, leaseID, method, path, body, out); ok {
			return err2
		}
	}
	return err
}

// fanOutDo tries the request against every up host the base endpoint
// lists at /v0/fleet/hosts, returning the first successful answer (or,
// after all hosts were tried, the first non-404 error), and caches the
// answering host as the lease's route so follow-up calls (and
// AddrForLease) go there directly.
// Restricted to idempotent GETs — the lease ID is a capability token, so
// mutating requests are never sprayed across non-owning hosts. ok is
// false when the base endpoint serves no host list (not a broker), the
// method is not GET, or no host gave a usable answer.
func (c *Client) fanOutDo(ctx context.Context, leaseID, method, path string, body, out any) (bool, error) {
	if method != http.MethodGet {
		return false, nil
	}
	hosts, err := c.FleetHosts(ctx)
	if err != nil {
		return false, nil
	}
	var firstErr error
	for _, h := range hosts {
		if !h.Up || h.Addr == "" {
			continue
		}
		hc := c.hostClient(normalizeAddr(h.Addr))
		herr := hc.do(ctx, method, path, body, out)
		if herr == nil {
			c.noteRoute(leaseID, h.Addr)
			return true, nil
		}
		// Any error means this host cannot answer (doesn't know the
		// run, lacks the route, is broken); keep looking — the owner
		// may be later in the list — but remember the first error that
		// wasn't a plain "I don't know it".
		var ae *APIError
		if firstErr == nil && (!errors.As(herr, &ae) || ae.Status != http.StatusNotFound) {
			var ce *ConnError
			if !errors.As(herr, &ce) {
				firstErr = herr
			}
		}
	}
	if firstErr != nil {
		return true, firstErr
	}
	return false, nil
}

// leaseControlDo issues a lease control op (get/renew/release) at the
// base endpoint — through a broker when there is one, keeping its lease
// table fresh — and falls back to the owning daemon when the broker is
// unreachable or has forgotten the lease (e.g. after a broker restart).
func (c *Client) leaseControlDo(ctx context.Context, leaseID, method, path string, body any, l *proto.Lease) error {
	err := c.do(ctx, method, path, body, l)
	if err != nil {
		lc := c.forLease(leaseID)
		var ce *ConnError
		var ae *APIError
		retryable := errors.As(err, &ce) ||
			(errors.As(err, &ae) && ae.Status == http.StatusNotFound)
		if lc != c && retryable {
			// The daemon's answer (success or its own error) is the truth
			// about the lease; the broker merely lost its routing entry.
			err = lc.do(ctx, method, path, body, l)
		}
	}
	if err == nil && l != nil {
		c.noteLease(*l)
	}
	return err
}
