package broker

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// decisionRingCap bounds the placement-decision ring buffer served at
// GET /v0/fleet/placements.
const decisionRingCap = 256

// tierNames maps placement tiers to their wire names.
var tierNames = map[int]string{
	tierWarm:      "warm",
	tierHeadroom:  "headroom",
	tierSaturated: "saturated",
}

// CandidateExplain is one host's rank in a placement decision: the inputs
// candidateHosts ordered it by, snapshotted at decision time.
type CandidateExplain struct {
	Host string `json:"host"`
	Tier string `json:"tier"`
	// EffectiveLoad is the load the ranking used (daemon-truth when the
	// daemon reports status, the broker-local counter otherwise).
	EffectiveLoad int `json:"effective_load"`
	// WarmMatch reports a parked-warm or Booted target carrying all
	// requested labels.
	WarmMatch bool `json:"warm_match"`
	// WarmIdle counts parked matching targets (the within-tier steering
	// key: deeper idle warm capacity attracts demand first).
	WarmIdle int `json:"warm_idle"`
	// HasStats is false for daemons that don't serve /v0/status; Parked
	// and Headroom are then zero/absent.
	HasStats bool `json:"has_stats"`
	Parked   int  `json:"parked,omitempty"`
	// Headroom is max_booted_running − running − boots_in_flight;
	// -1 means uncapped (no warm pool configured).
	Headroom int `json:"headroom,omitempty"`
}

// PlacementDecision records why one federated acquire went where it did.
type PlacementDecision struct {
	At     time.Time `json:"at"`
	Labels []string  `json:"labels"`
	Class  string    `json:"class"`
	// Outcome is "active", "queued", or an error code ("no_match",
	// "unavailable", "error").
	Outcome string `json:"outcome"`
	// Host is the host that won the placement; empty on error outcomes.
	Host string `json:"host,omitempty"`
	// Tier is the winning host's tier at decision time.
	Tier string `json:"tier,omitempty"`
	// LeaseID is a short one-way digest of the granted lease's ID
	// (lease IDs are capability tokens): the ID's holder can compute
	// the same digest to correlate, nothing of the raw ID leaks.
	LeaseID string `json:"lease_id,omitempty"`
	// Candidates is the ranked candidate list the decision walked, best
	// first.
	Candidates []CandidateExplain `json:"candidates"`
}

// decisionLog is a fixed-capacity ring of recent placement decisions.
type decisionLog struct {
	mu   sync.Mutex
	buf  []PlacementDecision
	next int
	full bool
}

func (d *decisionLog) add(pd PlacementDecision) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.buf == nil {
		d.buf = make([]PlacementDecision, decisionRingCap)
	}
	d.buf[d.next] = pd
	d.next = (d.next + 1) % len(d.buf)
	if d.next == 0 {
		d.full = true
	}
}

// recent returns up to n decisions, newest first.
func (d *decisionLog) recent(n int) []PlacementDecision {
	d.mu.Lock()
	defer d.mu.Unlock()
	size := d.next
	if d.full {
		size = len(d.buf)
	}
	if n <= 0 || n > size {
		n = size
	}
	out := make([]PlacementDecision, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, d.buf[(d.next-i+len(d.buf))%len(d.buf)])
	}
	return out
}

// since returns all decisions at or after t, newest first.
func (d *decisionLog) since(t time.Time) []PlacementDecision {
	all := d.recent(0)
	out := all[:0]
	for _, pd := range all {
		if !pd.At.Before(t) {
			out = append(out, pd)
		}
	}
	return out
}

// handleFleetPlacements serves GET /v0/fleet/placements: the most recent
// placement decisions (newest first), each with the ranked candidate list
// it walked — tier, effective load, warm hits — so "why did this lease go
// there?" is answerable after the fact. ?n= bounds the count.
func (b *Broker) handleFleetPlacements(w http.ResponseWriter, r *http.Request) {
	n := 0
	if v := r.URL.Query().Get("n"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, proto.ErrBadRequest, "n must be a non-negative integer")
			return
		}
		n = parsed
	}
	writeJSON(w, http.StatusOK, map[string]any{"placements": b.decisions.recent(n)})
}

// handleFleetHints serves GET /v0/fleet/hints: the broker's current
// per-host warm-pool advice, computed from windowed demand stats. Daemons
// (or operators) may poll this instead of — or in addition to — the
// broker's own POST /v0/pool/advise pushes.
func (b *Broker) handleFleetHints(w http.ResponseWriter, r *http.Request) {
	hints := b.computeHints(time.Now())
	if hints == nil {
		hints = []HostHints{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window_seconds": int(b.demandWindow / time.Second),
		"hosts":          hints,
	})
}
