package broker

import (
	"context"
	"net/http"
	"sync"

	"github.com/BariBariGood/manzanas/proto"
)

// handleListTargets serves GET /v0/targets: the union of all healthy
// daemons' targets, each annotated with its owning host name and the
// host's extra labels. A host whose live fetch fails falls back to its
// cached target list — the same list scheduling uses — so the federated
// view stays consistent with lease placement.
func (b *Broker) handleListTargets(w http.ResponseWriter, r *http.Request) {
	type result struct {
		idx     int
		targets []proto.Target
	}
	results := make([]result, 0, len(b.hosts))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for i, h := range b.hosts {
		if !h.isUp() {
			continue
		}
		wg.Add(1)
		go func(i int, h *host) {
			defer wg.Done()
			// Bound the per-host fetch so one hung host can't stall
			// the whole listing past the cached-list fallback.
			fctx, cancel := context.WithTimeout(r.Context(), b.probeTimeout)
			targets, err := b.fetchTargets(fctx, h)
			cancel()
			if err != nil {
				b.log.Warn("targets fetch failed, serving cached list", "host", h.cfg.Name, "err", err)
				targets = h.cachedTargets()
			} else {
				// Refresh the cache scheduling matches against, so a
				// target advertised here is immediately leasable.
				h.setCachedTargets(targets)
			}
			mu.Lock()
			results = append(results, result{i, targets})
			mu.Unlock()
		}(i, h)
	}
	wg.Wait()

	// Stable host order regardless of response arrival.
	merged := []proto.Target{}
	for _, want := range b.hosts {
		for _, res := range results {
			if b.hosts[res.idx] == want {
				merged = append(merged, res.targets...)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": merged})
}

// fetchTargets lists one daemon's targets, annotated with the host name
// and the host's configured extra labels.
func (b *Broker) fetchTargets(ctx context.Context, h *host) ([]proto.Target, error) {
	var resp struct {
		Targets []proto.Target `json:"targets"`
	}
	if err := b.client.getJSON(ctx, h.cfg.Token, h.cfg.Addr+"/v0/targets", &resp); err != nil {
		return nil, err
	}
	for i := range resp.Targets {
		resp.Targets[i].Host = h.cfg.Name
		resp.Targets[i].Labels = append(resp.Targets[i].Labels, h.cfg.Labels...)
	}
	return resp.Targets, nil
}
