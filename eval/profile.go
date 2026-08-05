package eval

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// TimingProfile adapts scenario timing to a host class. Scenario timeouts
// in the repo are tuned on the M3; slower Intel hosts need both longer
// budgets (steps take longer) and slower wait_* polling (each observe is
// expensive, so a tight interval thrashes the target).
type TimingProfile struct {
	Name string `json:"name"`
	// TimeoutScale multiplies client-side budgets: step timeouts, the
	// scenario default timeout, and the lease acquire timeout.
	TimeoutScale float64 `json:"timeout_scale"`
	// WaitScale multiplies the daemon-side polling knobs of wait_* action
	// payloads (timeout_ms, interval_ms, for_ms).
	WaitScale float64 `json:"wait_scale"`
}

// Builtin timing profiles. The M3 profile is the identity (scenarios are
// tuned on it); the Intel profile was calibrated on the fleet's Intel
// boxes (2017 MBP, 4-core work Mac), where boot, observe, and
// snapshot/restore run roughly 2-4x slower than the M3.
var profiles = map[string]TimingProfile{
	"m3":    {Name: "m3", TimeoutScale: 1, WaitScale: 1},
	"intel": {Name: "intel", TimeoutScale: 3, WaitScale: 2},
}

// ProfileByName returns a builtin timing profile.
func ProfileByName(name string) (TimingProfile, error) {
	p, ok := profiles[strings.ToLower(name)]
	if !ok {
		var names []string
		for n := range profiles {
			names = append(names, n)
		}
		sort.Strings(names)
		return TimingProfile{}, fmt.Errorf("unknown timing profile %q (have: %s)", name, strings.Join(names, ", "))
	}
	return p, nil
}

// scaleTimeout applies TimeoutScale to a client-side budget. An unset
// (non-positive) multiplier is the identity, so a partially-filled
// profile never collapses budgets to zero.
func (p TimingProfile) scaleTimeout(d time.Duration) time.Duration {
	if p.TimeoutScale <= 0 || p.TimeoutScale == 1 || d <= 0 {
		return d
	}
	return time.Duration(math.Round(float64(d) * p.TimeoutScale))
}

// waitPayloadKeys are the polling knobs scaled by WaitScale in wait_*
// action payloads.
var waitPayloadKeys = map[string]bool{"timeout_ms": true, "interval_ms": true, "for_ms": true}

// maxWaitTimeoutMS mirrors the daemon's maxWaitTimeout clamp
// (internal/actions/wait.go): timeout_ms above it is silently reduced
// server-side, so scaling past it buys nothing.
const maxWaitTimeoutMS = 2 * 60 * 1000

// defaultWaitTimeoutMS mirrors the daemon's per-kind default budget when
// a wait_* payload omits timeout_ms: wait_tree_stable uses
// defaultStableTimeout (internal/actions/wait_stable.go), everything
// else defaultWaitTimeout (internal/actions/wait.go).
func defaultWaitTimeoutMS(kind string) float64 {
	if kind == "wait_tree_stable" {
		return 15 * 1000
	}
	return 10 * 1000
}

// scaleWaitPayload returns the payload with polling knobs scaled by
// WaitScale when kind is a wait_* action. When scaling timeout_ms would
// exceed the daemon's clamp, the whole payload uses the largest factor
// that stays within it, so interval_ms grows in proportion to the real
// budget and the poll count is preserved; when timeout_ms is absent the
// daemon's default budget is materialized and scaled for the same
// reason. The original map is never mutated; when nothing changes it is
// returned as-is.
func (p TimingProfile) scaleWaitPayload(kind string, payload map[string]any) map[string]any {
	if p.WaitScale <= 0 || p.WaitScale == 1 || !strings.HasPrefix(kind, "wait_") {
		return payload
	}
	_, hasTimeout := payload["timeout_ms"]
	t, ok := toFloat(payload["timeout_ms"])
	if !ok {
		t = defaultWaitTimeoutMS(kind)
	}
	scale := p.WaitScale
	if t > 0 && t*scale > maxWaitTimeoutMS {
		scale = maxWaitTimeoutMS / t
		if scale < 1 {
			scale = 1
		}
	}
	if scale == 1 {
		return payload
	}
	scaled := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		if waitPayloadKeys[k] {
			if n, ok := toFloat(v); ok {
				scaled[k] = math.Round(n * scale)
				continue
			}
		}
		scaled[k] = v
	}
	if !hasTimeout {
		scaled["timeout_ms"] = math.Round(t * scale)
	}
	return scaled
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	}
	return 0, false
}
