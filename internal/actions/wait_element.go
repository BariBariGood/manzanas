package actions

import (
	"context"
	"errors"
	"time"
)

// handleWaitForElement polls the a11y tree until an element matching the
// payload predicate appears (default) or disappears (absent:true), or the
// timeout budget runs out. The response carries the matched element with
// its frame so the caller can tap it without a second observe round-trip.
func handleWaitForElement(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	return elemWaitFor(ctx, b, udid, p)
}

func elemWaitFor(ctx context.Context, d elementDriver, udid string, p map[string]any) (map[string]any, error) {
	pr, err := predicateFromPayload(p)
	if err != nil {
		return nil, err
	}
	absent, _ := p["absent"].(bool)
	refresh, err := boolFlag(p, "refresh", false)
	if err != nil {
		return nil, err
	}
	timeout, interval, err := waitParams(p, defaultWaitTimeout)
	if err != nil {
		return nil, err
	}

	var found *Node
	var nodes []*Node
	polls, elapsed, err := pollUntil(ctx, timeout, interval, func(ctx context.Context) error {
		obs, err := d.observeOnce(ctx, udid, refresh)
		if err != nil {
			return err
		}
		nodes = obs.nodes
		hit := pr.findBest(obs.nodes, obs.viewport)
		if absent {
			if hit != nil {
				return errNotYet
			}
			return nil
		}
		if hit == nil {
			return errNotYet
		}
		found = hit
		return nil
	})
	if err != nil {
		if errors.Is(err, errNotYet) || errors.Is(err, context.DeadlineExceeded) {
			verb := "appear"
			if absent {
				verb = "disappear"
			}
			return nil, timeoutErr("element (%s) did not %s within the %s budget (%d poll(s), %s elapsed)", pr, verb, timeout, polls, elapsed.Round(time.Millisecond))
		}
		return nil, err
	}

	res := map[string]any{
		"elapsed_ms": elapsed.Milliseconds(),
		"polls":      polls,
		"hash":       TreeHash(nodes),
	}
	if absent {
		res["absent"] = true
	} else {
		res["element"] = leafCopy(found)
	}
	return res, nil
}

// leafCopy returns the node without its subtree, keeping the response
// token-cheap; the caller asked for one element, not another full observe.
func leafCopy(n *Node) *Node {
	c := *n
	c.Children = nil
	return &c
}
