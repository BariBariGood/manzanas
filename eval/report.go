package eval

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Report is the aggregated benchmark report across scenarios.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	DaemonURL   string    `json:"daemon_url"`
	// Runs is the requested run count per scenario; a scenario's own
	// `runs` field reports how many actually executed (fewer on interrupt).
	Runs      int               `json:"runs_requested"`
	Scenarios []ScenarioSummary `json:"scenarios"`
}

// ScenarioSummary is the per-scenario aggregation.
type ScenarioSummary struct {
	Name            string  `json:"name"`
	Description     string  `json:"description,omitempty"`
	Runs            int     `json:"runs"`
	Passed          int     `json:"passed"`
	DeterminismRate float64 `json:"determinism_rate"` // passed/runs
	// HashConsistent reports whether every saved tree hash was identical
	// across all passing runs (nil if the scenario saves no hashes).
	HashConsistent *bool         `json:"hash_consistent,omitempty"`
	FlakySteps     []string      `json:"flaky_steps,omitempty"` // passed in some runs, failed in others
	Steps          []StepSummary `json:"steps"`
	RunResults     []RunResult   `json:"run_results"`
}

// StepSummary is the per-step latency + pass aggregation across runs.
type StepSummary struct {
	Step     string  `json:"step"`
	Op       string  `json:"op"`
	Executed int     `json:"executed"`
	Passed   int     `json:"passed"`
	P50MS    float64 `json:"p50_ms"`
	P90MS    float64 `json:"p90_ms"`
	MaxMS    float64 `json:"max_ms"`
}

// BuildReport aggregates scenario results into a report.
func BuildReport(daemonURL string, runs int, results []*ScenarioResult) *Report {
	rep := &Report{
		GeneratedAt: time.Now().UTC(),
		DaemonURL:   daemonURL,
		Runs:        runs,
	}
	for _, sr := range results {
		rep.Scenarios = append(rep.Scenarios, summarize(sr))
	}
	return rep
}

func summarize(sr *ScenarioResult) ScenarioSummary {
	sum := ScenarioSummary{
		Name:        sr.Scenario.Name,
		Description: sr.Scenario.Description,
		Runs:        len(sr.Runs),
		RunResults:  sr.Runs,
	}
	for _, rr := range sr.Runs {
		if rr.OK {
			sum.Passed++
		}
	}
	if sum.Runs > 0 {
		sum.DeterminismRate = float64(sum.Passed) / float64(sum.Runs)
	}
	sum.HashConsistent = hashConsistency(sr.Runs)
	sum.Steps, sum.FlakySteps = stepSummaries(sr.Runs)
	return sum
}

// hashConsistency checks that each named saved hash is identical across all
// passing runs. Returns nil when no passing run saved any hash.
func hashConsistency(runs []RunResult) *bool {
	first := map[string]string{}
	sawAny := false
	consistent := true
	for _, rr := range runs {
		if !rr.OK {
			continue
		}
		for name, hash := range rr.SavedHashes {
			sawAny = true
			if prev, ok := first[name]; !ok {
				first[name] = hash
			} else if prev != hash {
				consistent = false
			}
		}
	}
	if !sawAny {
		return nil
	}
	return &consistent
}

func stepSummaries(runs []RunResult) ([]StepSummary, []string) {
	type agg struct {
		op        string
		latencies []float64
		executed  int
		passed    int
		order     int
	}
	byStep := map[string]*agg{}
	order := 0
	for _, rr := range runs {
		for _, st := range rr.Steps {
			a, ok := byStep[st.Step]
			if !ok {
				a = &agg{op: st.Op, order: order}
				order++
				byStep[st.Step] = a
			}
			a.executed++
			a.latencies = append(a.latencies, st.LatencyMS)
			if st.OK {
				a.passed++
			}
		}
	}
	names := make([]string, 0, len(byStep))
	for name := range byStep {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return byStep[names[i]].order < byStep[names[j]].order })

	var steps []StepSummary
	var flaky []string
	for _, name := range names {
		a := byStep[name]
		steps = append(steps, StepSummary{
			Step:     name,
			Op:       a.op,
			Executed: a.executed,
			Passed:   a.passed,
			P50MS:    percentile(a.latencies, 50),
			P90MS:    percentile(a.latencies, 90),
			MaxMS:    percentile(a.latencies, 100),
		})
		if a.passed > 0 && a.passed < a.executed {
			flaky = append(flaky, name)
		}
	}
	return steps, flaky
}

// percentile returns the p-th percentile (nearest-rank) of values.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	rank := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	return sorted[rank]
}

// JSON renders the report as indented JSON.
func (r *Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// Markdown renders the report as a human-readable markdown document.
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# manzanasd eval report\n\n")
	fmt.Fprintf(&b, "- Generated: %s\n", r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- Daemon: `%s`\n", r.DaemonURL)
	fmt.Fprintf(&b, "- Runs per scenario (requested): %d\n\n", r.Runs)

	fmt.Fprintf(&b, "## Summary\n\n")
	fmt.Fprintf(&b, "| Scenario | Runs | Passed | Determinism | Hash-consistent | Flaky steps |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|\n")
	for _, s := range r.Scenarios {
		hc := "n/a"
		if s.HashConsistent != nil {
			hc = fmt.Sprintf("%v", *s.HashConsistent)
		}
		flaky := "-"
		if len(s.FlakySteps) > 0 {
			flaky = strings.Join(s.FlakySteps, ", ")
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %.0f%% | %s | %s |\n",
			s.Name, s.Runs, s.Passed, s.DeterminismRate*100, hc, flaky)
	}
	b.WriteString("\n")

	for _, s := range r.Scenarios {
		fmt.Fprintf(&b, "## Scenario: %s\n\n", s.Name)
		if s.Description != "" {
			fmt.Fprintf(&b, "%s\n\n", s.Description)
		}
		fmt.Fprintf(&b, "### Step latencies\n\n")
		fmt.Fprintf(&b, "| Step | Op | Executed | Passed | p50 (ms) | p90 (ms) | max (ms) |\n")
		fmt.Fprintf(&b, "|---|---|---|---|---|---|---|\n")
		for _, st := range s.Steps {
			fmt.Fprintf(&b, "| %s | %s | %d | %d | %.0f | %.0f | %.0f |\n",
				st.Step, st.Op, st.Executed, st.Passed, st.P50MS, st.P90MS, st.MaxMS)
		}
		b.WriteString("\n### Runs\n\n")
		for _, rr := range s.RunResults {
			status := "PASS"
			if !rr.OK {
				status = "FAIL"
			}
			fmt.Fprintf(&b, "- run %d: **%s** (%.1fs, target `%s`)", rr.Run, status, rr.DurationMS/1000, rr.TargetUDID)
			if rr.Error != "" {
				fmt.Fprintf(&b, " — %s", rr.Error)
			}
			for _, st := range rr.Steps {
				if !st.OK {
					fmt.Fprintf(&b, " — step `%s`: %s", st.Step, st.Error)
				}
			}
			if len(rr.Artifacts) > 0 {
				names := make([]string, len(rr.Artifacts))
				for i, a := range rr.Artifacts {
					names[i] = filepath.Base(a)
				}
				fmt.Fprintf(&b, " — artifacts: %s", strings.Join(names, ", "))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}
