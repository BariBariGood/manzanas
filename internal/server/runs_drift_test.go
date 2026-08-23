package server

import (
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

func boolPtr(b bool) *bool { return &b }

func TestComputeVerdict(t *testing.T) {
	exp := &proto.RunExpect{Texts: []string{"welcome"}}
	cases := []struct {
		name    string
		steps   []proto.RunStep
		results []proto.RunStepResult
		want    proto.RunVerdict
	}{
		{
			name:    "no signals at all",
			steps:   []proto.RunStep{{Action: "tap"}, {Action: "type"}},
			results: []proto.RunStepResult{{Action: "tap", Status: proto.StepOK}, {Action: "type", Status: proto.StepOK}},
			want:    proto.VerdictUnverified,
		},
		{
			name:  "every mutating step verified",
			steps: []proto.RunStep{{Action: "tap"}, {Action: "tap"}},
			results: []proto.RunStepResult{
				{Action: "tap", Status: proto.StepOK, Verified: boolPtr(true)},
				{Action: "tap", Status: proto.StepOK, Verified: boolPtr(true)},
			},
			want: proto.VerdictTrusted,
		},
		{
			name:  "partial verification coverage",
			steps: []proto.RunStep{{Action: "tap"}, {Action: "type"}},
			results: []proto.RunStepResult{
				{Action: "tap", Status: proto.StepOK, Verified: boolPtr(true)},
				{Action: "type", Status: proto.StepOK},
			},
			want: proto.VerdictDegraded,
		},
		{
			name:  "verified:false is a failed verification",
			steps: []proto.RunStep{{Action: "tap"}},
			results: []proto.RunStepResult{
				{Action: "tap", Status: proto.StepOK, Verified: boolPtr(false)},
			},
			want: proto.VerdictDegraded,
		},
		{
			name:  "following passing assert verifies the mutating step",
			steps: []proto.RunStep{{Action: "tap"}, {Action: "assert_text"}},
			results: []proto.RunStepResult{
				{Action: "tap", Status: proto.StepOK},
				{Action: "assert_text", Status: proto.StepOK},
			},
			want: proto.VerdictTrusted,
		},
		{
			name:  "failing assert degrades",
			steps: []proto.RunStep{{Action: "tap"}, {Action: "assert_text"}},
			results: []proto.RunStepResult{
				{Action: "tap", Status: proto.StepOK},
				{Action: "assert_text", Status: proto.StepError},
			},
			want: proto.VerdictDegraded,
		},
		{
			name:  "passing expect check verifies",
			steps: []proto.RunStep{{Action: "tap", Expect: exp}},
			results: []proto.RunStepResult{
				{Action: "tap", Status: proto.StepOK},
			},
			want: proto.VerdictTrusted,
		},
		{
			name:  "drift caps at degraded",
			steps: []proto.RunStep{{Action: "tap", Expect: exp}, {Action: "tap", Expect: exp}},
			results: []proto.RunStepResult{
				{Action: "tap", Status: proto.StepOK},
				{Action: "tap", Status: proto.StepOK, Drifted: true, DriftDetail: "text-set jaccard 0.10 < 0.70"},
			},
			want: proto.VerdictDegraded,
		},
		{
			name:  "skipped drift check is not a signal",
			steps: []proto.RunStep{{Action: "tap", Expect: exp}},
			results: []proto.RunStepResult{
				{Action: "tap", Status: proto.StepOK, DriftDetail: "drift check skipped: observe failed"},
			},
			want: proto.VerdictUnverified,
		},
		{
			name:  "drift on a read-only step caps at degraded",
			steps: []proto.RunStep{{Action: "tap"}, {Action: "wait_for_element", Expect: exp}},
			results: []proto.RunStepResult{
				{Action: "tap", Status: proto.StepOK, Verified: boolPtr(true)},
				{Action: "wait_for_element", Status: proto.StepOK, Drifted: true, DriftDetail: "text-set jaccard 0.10 < 0.70"},
			},
			want: proto.VerdictDegraded,
		},
		{
			name:  "read-only steps need no verification",
			steps: []proto.RunStep{{Action: "tap"}, {Action: "screenshot"}},
			results: []proto.RunStepResult{
				{Action: "tap", Status: proto.StepOK, Verified: boolPtr(true)},
				{Action: "screenshot", Status: proto.StepOK},
			},
			want: proto.VerdictTrusted,
		},
		{
			name:  "failed mutating step leaves coverage incomplete",
			steps: []proto.RunStep{{Action: "tap"}, {Action: "tap"}},
			results: []proto.RunStepResult{
				{Action: "tap", Status: proto.StepOK, Verified: boolPtr(true)},
				{Action: "tap", Status: proto.StepError},
			},
			want: proto.VerdictDegraded,
		},
		{
			name:  "skipped steps ignored",
			steps: []proto.RunStep{{Action: "tap"}, {Action: "tap"}},
			results: []proto.RunStepResult{
				{Action: "tap", Status: proto.StepOK, Verified: boolPtr(true)},
				{Action: "tap", Status: proto.StepSkipped},
			},
			want: proto.VerdictTrusted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := computeVerdict(tc.steps, tc.results); got != tc.want {
				t.Fatalf("verdict = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestTextJaccard(t *testing.T) {
	set := func(items ...string) map[string]struct{} {
		m := make(map[string]struct{})
		for _, s := range items {
			m[s] = struct{}{}
		}
		return m
	}
	if j := textJaccard(set(), set()); j != 1.0 {
		t.Fatalf("empty sets = %v, want 1.0", j)
	}
	if j := textJaccard(set("a1", "b2"), set("a1", "b2")); j != 1.0 {
		t.Fatalf("identical = %v, want 1.0", j)
	}
	if j := textJaccard(set("a1", "b2"), set("a1", "c3")); j != 1.0/3.0 {
		t.Fatalf("overlap = %v, want 1/3", j)
	}
	if j := textJaccard(set("a1"), set("b2")); j != 0 {
		t.Fatalf("disjoint = %v, want 0", j)
	}
}
