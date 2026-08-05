package warm

import "runtime"

// CapacityClass caps a host's simulator commitments. Values come from
// fleet measurements: Intel boxes melt above 2 running sims (boot storms
// drove 1-min load past 500), Apple Silicon handles 4 headless slimmed
// sims comfortably.
type CapacityClass struct {
	// MaxBootedRunning caps sims that are Booted AND not parked (parked
	// sims are SIGSTOPped and cost ~0 CPU, so they don't count).
	MaxBootedRunning int
	// MaxParked caps sims held parked in the pool.
	MaxParked int
	// MaxConcurrentBoots serializes boots: Intel must boot one at a time.
	MaxConcurrentBoots int
}

var (
	// IntelClass: {2 running, 6 parked, 1 boot at a time}.
	IntelClass = CapacityClass{MaxBootedRunning: 2, MaxParked: 6, MaxConcurrentBoots: 1}
	// AppleSiliconClass: {4 running, 4 parked, 2 boots at a time}.
	AppleSiliconClass = CapacityClass{MaxBootedRunning: 4, MaxParked: 4, MaxConcurrentBoots: 2}
)

// DetectClass picks the capacity class for the current host architecture.
func DetectClass() CapacityClass {
	if runtime.GOARCH == "arm64" {
		return AppleSiliconClass
	}
	return IntelClass
}
