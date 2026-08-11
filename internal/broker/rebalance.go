package broker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// DefaultDemandWindow is how far back demand stats look when computing
// warm-pool rebalancing hints.
const DefaultDemandWindow = 10 * time.Minute

// growColdThreshold is how many cold (non-warm-tier) placements of one
// class within the demand window it takes before matching hosts are
// advised to grow their pools for it.
const growColdThreshold = 3

// shrinkMinDemand is how many total placements the window must have seen
// before an entirely idle warm pool draws shrink advice — a quiet fleet
// says nothing about the pool being oversized.
const shrinkMinDemand = 3

// classKey normalizes a requested label set into a stable demand-class
// key (sorted, comma-joined).
func classKey(labels []string) string {
	sorted := append([]string{}, labels...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// HostHints is one host's current warm-pool advice.
type HostHints struct {
	Host    string                  `json:"host"`
	Classes []proto.PoolClassAdvice `json:"classes"`
}

// computeHints derives per-host warm-pool advice from the placement
// decisions inside the demand window:
//
//   - grow: a class fell to cold tiers (headroom/saturated) at least
//     growColdThreshold times — every up host whose target cache matches
//     the class is advised to keep it warm.
//   - shrink: a host holds parked warm capacity that served zero warm
//     placements all window, while the fleet saw real demand — the pool
//     may be warming the wrong things.
//
// Hints are advisory: daemons keep final say via their own capacity class
// and safety gates, and old daemons that know nothing about advice keep
// working unchanged.
func (b *Broker) computeHints(now time.Time) []HostHints {
	window := b.decisions.since(now.Add(-b.demandWindow))

	type classStats struct {
		labels   []string
		cold     int
		warmHits map[string]int // per host
	}
	classes := map[string]*classStats{}
	warmHitsByHost := map[string]int{}
	placed := 0
	for _, pd := range window {
		if pd.Outcome != "active" && pd.Outcome != "queued" {
			continue
		}
		placed++
		cs := classes[pd.Class]
		if cs == nil {
			cs = &classStats{labels: pd.Labels, warmHits: map[string]int{}}
			classes[pd.Class] = cs
		}
		// Only an instantly-granted lease on a warm-tier host counts as
		// a warm hit; a queued one waited regardless of the tier, which
		// is exactly the demand grow advice is for.
		if pd.Tier == tierNames[tierWarm] && pd.Outcome == string(proto.LeaseActive) {
			cs.warmHits[pd.Host]++
			warmHitsByHost[pd.Host]++
		} else {
			cs.cold++
		}
	}

	var out []HostHints
	for _, h := range b.hosts {
		if !h.isUp() {
			continue
		}
		var advice []proto.PoolClassAdvice
		for _, key := range sortedKeys(classes) {
			cs := classes[key]
			if cs.cold < growColdThreshold || !hostMatches(h, cs.labels) {
				continue
			}
			// Daemons only know target-derived labels: strip broker-only
			// host-level labels (as acquires do) so the advised class is
			// one the daemon can actually match. A class that was pure
			// host pinning has nothing target-specific to keep warm.
			labels := stripHostLabels(cs.labels, h)
			if len(labels) == 0 {
				continue
			}
			advice = append(advice, proto.PoolClassAdvice{
				Labels:         labels,
				Action:         proto.AdviceGrow,
				ColdPlacements: cs.cold,
				WarmHits:       cs.warmHits[h.cfg.Name],
				Reason: fmt.Sprintf("%d placements of class %q fell to cold tiers in the last %s",
					cs.cold, key, b.demandWindow),
			})
		}
		// Shrink only when the host was up for the whole demand window
		// (a host that was down or newly added has trivially zero warm
		// hits) and isn't simultaneously being asked to grow.
		upSince, upOK := h.upSinceTime()
		if st, ok := h.statsSnapshot(); ok && st.Parked > 0 && len(advice) == 0 &&
			upOK && !upSince.After(now.Add(-b.demandWindow)) &&
			warmHitsByHost[h.cfg.Name] == 0 && placed >= shrinkMinDemand {
			advice = append(advice, proto.PoolClassAdvice{
				Action: proto.AdviceShrink,
				Reason: fmt.Sprintf("%d parked sims served no warm placement across %d placements in the last %s",
					st.Parked, placed, b.demandWindow),
			})
		}
		if len(advice) > 0 {
			out = append(out, HostHints{Host: h.cfg.Name, Classes: advice})
		}
	}
	return out
}

func sortedKeys[V any](m map[string]*V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// adviseHosts pushes each up host's current hints to its daemon via
// POST /v0/pool/advise, once per change. Daemons without the endpoint
// (404/501 — old builds) are marked unsupported and skipped until they
// bounce; pushes are best-effort and never affect placement.
func (b *Broker) adviseHosts(ctx context.Context) {
	hints := b.computeHints(time.Now())
	byHost := map[string]HostHints{}
	for _, hh := range hints {
		byHost[hh.Host] = hh
	}
	for _, h := range b.hosts {
		if !h.isUp() || h.adviseUnsupportedNow() {
			continue
		}
		hh := byHost[h.cfg.Name]
		key := fmt.Sprintf("%+v", hh.Classes)
		last, pushed, resync := h.adviceState()
		if pushed && last == key {
			continue // unchanged since the last push
		}
		if !pushed && !resync && len(hh.Classes) == 0 {
			// Nothing to say and nothing said before — unless the host
			// bounced, where one push (even empty) re-syncs a daemon
			// that may still hold advice from before the blip.
			continue
		}
		req := proto.PoolAdviseRequest{
			Source:        "broker",
			WindowSeconds: int(b.demandWindow / time.Second),
			Classes:       hh.Classes,
		}
		actx, cancel := context.WithTimeout(ctx, b.probeTimeout)
		err := b.client.postJSON(actx, h.cfg.Token, h.cfg.Addr+"/v0/pool/advise", req, nil)
		cancel()
		if err == nil {
			h.setAdvicePushed(key)
			b.log.Info("pool advice pushed", "host", h.cfg.Name, "classes", len(hh.Classes))
			continue
		}
		var de *daemonError
		if errors.As(err, &de) && (de.Status == http.StatusNotFound || de.Status == http.StatusNotImplemented) {
			h.setAdviseUnsupported()
			continue
		}
		// Plain-text 404s from old daemons don't decode into daemonError.
		if strings.Contains(err.Error(), "unexpected status 404") ||
			strings.Contains(err.Error(), "unexpected status 501") {
			h.setAdviseUnsupported()
			continue
		}
		b.log.Warn("pool advice push failed", "host", h.cfg.Name, "err", err)
	}
}
