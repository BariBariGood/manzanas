package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"sort"
	"strconv"
	"strings"

	"github.com/BariBariGood/manzanas/internal/actions"
	"github.com/BariBariGood/manzanas/internal/imgutil"
	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/proto"
)

// Replay drift detection and run verdicts (PROTOCOL.md §9, docs/runs.md).
//
// A step's optional expect block declares its expected post-state: the
// screen's masked text set and/or a DHash fingerprint. After the step
// succeeds the engine re-observes the screen and compares; a mismatch
// marks the step result drifted (report-only) and caps the run verdict
// at degraded — or fails the run under spec strict_drift.
//
// The same observation, journaled at record time (spec
// record_expectations), is what `journal to-spec --with-expectations`
// turns back into expect blocks for the replay spec.

// Drift-check defaults.
const (
	// defaultJaccardMin is the minimum Jaccard overlap between the
	// expected and observed text sets (matches the scroll composites'
	// moved threshold tuning: real screen changes fall well below it).
	defaultJaccardMin = 0.7
	// defaultMaxHashDistance is the maximum accepted Hamming distance
	// between the expected and observed 64-bit DHash.
	defaultMaxHashDistance = 10
	// Volatile bands masked out of both the text set and the DHash,
	// as fractions of the viewport height (clock/status strip at the
	// top, home indicator at the bottom) — the same fractions the
	// scrolling composites mask.
	driftTopBandFrac    = 0.05
	driftBottomBandFrac = 0.03
	// driftMinRunLen drops normalized text runs shorter than this many
	// runes (OCR noise), mirroring the actions text-set normalization.
	driftMinRunLen = 2
)

// verdictAssertKinds are the assert step kinds whose passing execution
// counts as verification for the preceding mutating step.
var verdictAssertKinds = map[string]bool{
	"assert_text":     true,
	"assert_no_text":  true,
	"assert_template": true,
}

// mutatingStepKind reports whether an executed step kind mutates target
// state (anything that is not a pure-read observation). Batch steps are
// opaque and conservatively counted as mutating.
func mutatingStepKind(kind string) bool {
	return !journal.ReadOnlyKind(kind)
}

// stepObservation is one post-step screen observation: the masked text
// set plus the masked screen DHash (when the capture succeeded).
type stepObservation struct {
	texts    map[string]struct{}
	dhash    uint64
	hasHash  bool
	hashErr  string
	textsErr string
}

// observeStep captures the current screen state through the same action
// dispatch path the steps use (so the evidence is journaled normally).
// needTexts/needHash select which captures to spend.
func (s *Server) observeStep(ctx context.Context, leaseID string, needTexts, needHash bool) stepObservation {
	obs := stepObservation{}
	// The purpose marker tags the journaled entries as engine-injected
	// captures, so the spec exporter can tell them apart from user steps.
	if needTexts {
		res, wireErr := s.dispatchAction(ctx, proto.ActionRequest{
			LeaseID: leaseID, Kind: "observe",
			Payload: map[string]any{"purpose": journal.StepExpectationAction},
		})
		if wireErr != nil {
			obs.textsErr = wireErr.Message
		} else {
			nodes, _ := res.Result["tree"].([]*actions.Node)
			obs.texts = maskedTextSet(nodes)
		}
	}
	if needHash {
		res, wireErr := s.dispatchAction(ctx, proto.ActionRequest{
			LeaseID: leaseID, Kind: "screenshot",
			Payload: map[string]any{"format": "png", "purpose": journal.StepExpectationAction},
		})
		if wireErr != nil {
			obs.hashErr = wireErr.Message
		} else if img, err := decodeCapture(res.Result); err != nil {
			obs.hashErr = err.Error()
		} else {
			obs.dhash = maskedDHash(img)
			obs.hasHash = true
		}
	}
	return obs
}

// decodeCapture decodes a screenshot action result's inline image.
func decodeCapture(result map[string]any) (image.Image, error) {
	format, _ := result["format"].(string)
	if format == "" {
		format = "png"
	}
	b64, _ := result[format+"_base64"].(string)
	if b64 == "" {
		return nil, fmt.Errorf("screenshot result carried no inline %s image", format)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("screenshot base64 decode failed: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("screenshot decode failed: %v", err)
	}
	return img, nil
}

// maskedDHash fingerprints a capture with the volatile top/bottom bands
// masked out, so a ticking clock or home indicator does not perturb it.
func maskedDHash(img image.Image) uint64 {
	b := img.Bounds()
	h := b.Dy()
	masks := []image.Rectangle{
		image.Rect(b.Min.X, b.Min.Y, b.Max.X, b.Min.Y+int(float64(h)*driftTopBandFrac)),
		image.Rect(b.Min.X, b.Max.Y-int(float64(h)*driftBottomBandFrac), b.Max.X, b.Max.Y),
	}
	return imgutil.DHash(img, masks)
}

// maskedTextSet flattens an observed tree into a normalized text set with
// the volatile bands masked, mirroring the actions text-set semantics.
// The viewport is taken as the union of the tree's element frames.
func maskedTextSet(nodes []*actions.Node) map[string]struct{} {
	viewport := treeExtent(nodes)
	out := make(map[string]struct{})
	var walk func(ns []*actions.Node)
	walk = func(ns []*actions.Node) {
		for _, n := range ns {
			if n == nil {
				continue
			}
			if !inDriftBand(n.Frame, viewport) {
				for _, s := range []string{n.Label, n.Value, n.Placeholder} {
					if t := normalizeTextRun(s); t != "" {
						out[t] = struct{}{}
					}
				}
			}
			walk(n.Children)
		}
	}
	walk(nodes)
	return out
}

// treeExtent returns the union of all element frames (the viewport the
// volatile bands are sized against). Nil when no node carries a frame.
func treeExtent(nodes []*actions.Node) *actions.Frame {
	var ext *actions.Frame
	var walk func(ns []*actions.Node)
	walk = func(ns []*actions.Node) {
		for _, n := range ns {
			if n == nil {
				continue
			}
			if f := n.Frame; f != nil {
				if ext == nil {
					c := *f
					ext = &c
				} else {
					x0 := min(ext.X, f.X)
					y0 := min(ext.Y, f.Y)
					x1 := max(ext.X+ext.W, f.X+f.W)
					y1 := max(ext.Y+ext.H, f.Y+f.H)
					ext.X, ext.Y, ext.W, ext.H = x0, y0, x1-x0, y1-y0
				}
			}
			walk(n.Children)
		}
	}
	walk(nodes)
	return ext
}

// inDriftBand reports whether a frame's vertical center falls inside the
// masked top or bottom viewport band.
func inDriftBand(f, viewport *actions.Frame) bool {
	if f == nil || viewport == nil || viewport.H <= 0 {
		return false
	}
	cy := f.Y + f.H/2
	return cy < viewport.Y+viewport.H*driftTopBandFrac ||
		cy > viewport.Y+viewport.H*(1-driftBottomBandFrac)
}

// normalizeTextRun lowercases and trims one visible text run, dropping
// runs too short to be useful tokens.
func normalizeTextRun(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len([]rune(s)) < driftMinRunLen {
		return ""
	}
	return s
}

// textJaccard returns |a∩b| / |a∪b|; two empty sets are identical (1.0).
func textJaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	return float64(inter) / float64(union)
}

// checkExpect compares a step's expect block against the live post-step
// observation. Returns drifted plus a human-readable detail; a capture
// failure returns checkErr (the check is skipped, never a step failure).
func (s *Server) checkExpect(ctx context.Context, leaseID string, e *proto.RunExpect) (drifted bool, detail string, checkErr error) {
	obs := s.observeStep(ctx, leaseID, len(e.Texts) > 0, e.Fingerprint != "")
	var mismatches []string
	if len(e.Texts) > 0 {
		if obs.textsErr != "" {
			return false, "", fmt.Errorf("observe failed: %s", obs.textsErr)
		}
		want := make(map[string]struct{}, len(e.Texts))
		for _, t := range e.Texts {
			if n := normalizeTextRun(t); n != "" {
				want[n] = struct{}{}
			}
		}
		minJ := e.JaccardMin
		if minJ == 0 {
			minJ = defaultJaccardMin
		}
		if j := textJaccard(want, obs.texts); j < minJ {
			mismatches = append(mismatches,
				fmt.Sprintf("text-set jaccard %.2f < %.2f", j, minJ))
		}
	}
	if e.Fingerprint != "" {
		if obs.hashErr != "" {
			return false, "", fmt.Errorf("screenshot failed: %s", obs.hashErr)
		}
		want, err := strconv.ParseUint(e.Fingerprint, 16, 64)
		if err != nil {
			return false, "", fmt.Errorf("expect.fingerprint is not a 64-bit hex DHash: %v", err)
		}
		maxDist := defaultMaxHashDistance
		if e.MaxHashDistance != nil {
			maxDist = *e.MaxHashDistance
		}
		if d := imgutil.HashDistance(want, obs.dhash); d > maxDist {
			mismatches = append(mismatches,
				fmt.Sprintf("fingerprint distance %d > %d (want %s, got %016x)",
					d, maxDist, e.Fingerprint, obs.dhash))
		}
	}
	if len(mismatches) == 0 {
		return false, "", nil
	}
	return true, strings.Join(mismatches, "; "), nil
}

// recordStepExpectation journals the post-step observation (masked text
// set + DHash) as an "observation" entry, the raw material
// `journal to-spec --with-expectations` turns into expect blocks.
func (s *Server) recordStepExpectation(ctx context.Context, l proto.Lease, stepIndex int, action string) {
	obs := s.observeStep(ctx, l.ID, true, true)
	extra := map[string]any{"step_index": stepIndex, "step_action": action}
	if obs.textsErr == "" {
		texts := make([]string, 0, len(obs.texts))
		for t := range obs.texts {
			texts = append(texts, t)
		}
		sort.Strings(texts)
		extra["texts"] = texts
	} else {
		extra["texts_error"] = obs.textsErr
	}
	if obs.hasHash {
		extra["dhash"] = fmt.Sprintf("%016x", obs.dhash)
	} else if obs.hashErr != "" {
		extra["dhash_error"] = obs.hashErr
	}
	s.record(ctx, journal.Event{
		Kind: "observation", LeaseID: l.ID, AgentID: l.AgentID,
		Action: journal.StepExpectationAction, Status: "ok", Extra: extra,
	})
}

// computeVerdict grades a finished steps stage: trusted when every
// executed mutating step's postcondition was verified (verified:true, a
// passing expect check, or a following passing assert) and nothing
// failed verification; degraded when signals exist but coverage is
// incomplete or negative; unverified when the run carried no
// verification signals at all.
func computeVerdict(steps []proto.RunStep, results []proto.RunStepResult) proto.RunVerdict {
	signals := 0
	negatives := 0
	unverified := 0
	for i, res := range results {
		if i >= len(steps) {
			break
		}
		if res.Status == proto.StepSkipped {
			continue
		}
		// An expect check is a signal on any executed step that declared
		// one — including read-only steps — so drift always caps the
		// verdict at degraded.
		expectPassed := false
		if steps[i].Expect != nil && res.DriftDetail != "" && !res.Drifted {
			// The drift check could not run (capture failure): no signal.
		} else if steps[i].Expect != nil && res.Status == proto.StepOK {
			signals++
			if res.Drifted {
				negatives++
			} else {
				expectPassed = true
			}
		}
		if !mutatingStepKind(res.Action) {
			// Read-only steps have no postcondition of their own (a
			// passing assert is counted as its preceding mutating step's
			// verification below).
			continue
		}
		positive := expectPassed
		if res.Verified != nil {
			signals++
			if *res.Verified {
				positive = true
			} else {
				negatives++
			}
		}
		// A following assert (before the next mutating step) verifies
		// this step's postcondition.
		for j := i + 1; j < len(results); j++ {
			if mutatingStepKind(results[j].Action) {
				break
			}
			if !verdictAssertKinds[results[j].Action] || results[j].Status == proto.StepSkipped {
				continue
			}
			signals++
			if results[j].Status == proto.StepOK {
				positive = true
			} else {
				negatives++
			}
		}
		if !positive || res.Status != proto.StepOK {
			unverified++
		}
	}
	if signals == 0 {
		return proto.VerdictUnverified
	}
	if negatives == 0 && unverified == 0 {
		return proto.VerdictTrusted
	}
	return proto.VerdictDegraded
}
