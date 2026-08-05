package broker

import (
	"math"
	"sort"

	"github.com/BariBariGood/manzanas/proto"
)

// Placement tiers (warm-first with spillover). Within a tier, hosts are
// ordered by ascending effectiveLoad. Hosts without daemon stats (old
// builds) sit in the neutral tier so their relative order stays exactly
// the pre-status behavior.
const (
	// tierWarm: a matching target is parked-warm or already Booted —
	// granting there needs no cold boot (26ms thaw beats a multi-minute
	// boot).
	tierWarm = iota
	// tierHeadroom: a cold boot would be accepted — running headroom
	// left and the load/disk gates pass. Also the default for hosts
	// that report no stats.
	tierHeadroom
	// tierSaturated: an acquire is guaranteed to queue (no headroom, no
	// warm match) or the daemon would refuse the boot (gates failing) —
	// ranked last, never skipped: the daemon stays the final authority.
	tierSaturated
)

// tier ranks a host for a given label set. Tiering needs daemon stats;
// without them the host keeps today's least-loaded ordering (neutral).
func hostTier(h *host, labels []string) int {
	st, ok := h.statsSnapshot()
	if !ok {
		return tierHeadroom
	}
	if hostWarmMatch(h, labels) {
		return tierWarm
	}
	if runHeadroom(st) > 0 && st.Gates.LoadOK && st.Gates.DiskOK {
		return tierHeadroom
	}
	return tierSaturated
}

// hostWarmMatch reports whether any cached target carrying all requested
// labels is warm (parked in the daemon's pool) or already Booted.
func hostWarmMatch(h *host, labels []string) bool {
	for _, t := range h.cachedTargets() {
		if !t.Warm && t.State != proto.StateBooted {
			continue
		}
		all := append([]string{h.cfg.Name}, t.Labels...)
		if containsAll(all, labels) {
			return true
		}
	}
	return false
}

// runHeadroom is how many more cold boots the host can take:
// max_booted_running − running − boots_in_flight. An unreported cap
// (0, e.g. a daemon without a warm pool) means uncapped.
func runHeadroom(st proto.HostStatus) int {
	if st.Capacity.MaxBootedRunning <= 0 {
		return math.MaxInt
	}
	return st.Capacity.MaxBootedRunning - st.Running - st.BootsInFlight
}

// queuedDepth is the host's daemon-reported queue depth, used to pick
// the shallowest queue when every candidate would queue. Hosts without
// stats rank last (unknown depth).
func (h *host) queuedDepth() int {
	st, ok := h.statsSnapshot()
	if !ok {
		return math.MaxInt
	}
	return st.LeasesQueued
}

// warmIdleCount counts parked (warm, unclaimed) cached targets carrying
// all requested labels — the depth of matching warm capacity behind a
// warm match.
func warmIdleCount(h *host, labels []string) int {
	n := 0
	for _, t := range h.cachedTargets() {
		if !t.Warm {
			continue
		}
		all := append([]string{h.cfg.Name}, t.Labels...)
		if containsAll(all, labels) {
			n++
		}
	}
	return n
}

// rankedHost is one candidate with the inputs it was ordered by,
// snapshotted for placement explain.
type rankedHost struct {
	h        *host
	tier     int
	load     int
	warmIdle int
}

// rankCandidates returns the healthy hosts with at least one cached
// target matching all requested labels, ordered for placement:
// warm-first tier, then ascending effective load (daemon-truth
// active+queued counts when the daemon reports status, the broker-local
// counter otherwise), then descending idle warm depth — demand is steered
// toward the host with the most matching parked capacity, keeping thin
// warm pools free for their own classes — with a rotating tiebreak for
// fair spread.
func (b *Broker) rankCandidates(labels []string) []rankedHost {
	var out []rankedHost
	for _, h := range b.hosts {
		if h.isUp() && hostMatches(h, labels) {
			out = append(out, rankedHost{h, hostTier(h, labels), h.effectiveLoad(), warmIdleCount(h, labels)})
		}
	}
	rr := b.nextRR()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].tier != out[j].tier {
			return out[i].tier < out[j].tier
		}
		if out[i].load != out[j].load {
			return out[i].load < out[j].load
		}
		return out[i].warmIdle > out[j].warmIdle
	})
	// Rotate runs of equal rank so ties don't always land on the same host.
	for lo := 0; lo < len(out); {
		hi := lo
		for hi+1 < len(out) && out[hi+1].tier == out[lo].tier &&
			out[hi+1].load == out[lo].load && out[hi+1].warmIdle == out[lo].warmIdle {
			hi++
		}
		if n := hi - lo + 1; n > 1 {
			rotated := append([]rankedHost{}, out[lo:hi+1]...)
			k := rr % n
			copy(out[lo:hi+1], append(rotated[k:], rotated[:k]...))
		}
		lo = hi + 1
	}
	return out
}

// candidateHosts is rankCandidates reduced to the host order.
func (b *Broker) candidateHosts(labels []string) []*host {
	ranked := b.rankCandidates(labels)
	hosts := make([]*host, len(ranked))
	for i, r := range ranked {
		hosts[i] = r.h
	}
	return hosts
}

// explainCandidates snapshots a ranked candidate list for the placement
// decision log.
func explainCandidates(labels []string, ranked []rankedHost) []CandidateExplain {
	out := make([]CandidateExplain, 0, len(ranked))
	for _, r := range ranked {
		ce := CandidateExplain{
			Host:          r.h.cfg.Name,
			Tier:          tierNames[r.tier],
			EffectiveLoad: r.load,
			WarmMatch:     hostWarmMatch(r.h, labels),
			WarmIdle:      r.warmIdle,
		}
		if st, ok := r.h.statsSnapshot(); ok {
			ce.HasStats = true
			ce.Parked = st.Parked
			switch hr := runHeadroom(st); {
			case hr == math.MaxInt:
				ce.Headroom = -1
			case hr < 0:
				// Running can exceed the cap (sims booted outside
				// manzanasd, a lowered cap): clamp so a negative value
				// never collides with the -1 uncapped sentinel.
				ce.Headroom = 0
			default:
				ce.Headroom = hr
			}
		}
		out = append(out, ce)
	}
	return out
}
