package actions

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"time"

	"github.com/BariBariGood/manzanas/internal/elementmap"
	"github.com/BariBariGood/manzanas/internal/imgutil"
)

// Element-map fast path (#186): elements resolved through the a11y/OCR
// rungs are recorded into the on-disk element map
// (internal/elementmap), and a "map" rung — the ladder's FIRST rung —
// serves the cached frame directly on later resolutions when the current
// screen's DHash is within threshold of the fingerprint recorded at
// learn time. No observe round-trip; journal entries carry
// resolver:"map". A post-resolution failure (a tap the cached frame
// steered wrong) invalidates the entry so the next resolution re-learns
// through the slow rungs.

// resolverMapName is the fast-path rung's provenance name.
const resolverMapName = "map"

// mapRungBudget bounds one map-rung attempt (a screenshot + a hash).
const mapRungBudget = 1500 * time.Millisecond

// mapMaxHashDistance is the DHash Hamming-distance ceiling for a
// fingerprint hit: two captures of the same settled screen differ by ~0
// bits; unrelated screens differ by ~20-30. A small allowance absorbs
// antialiasing/compression jitter without matching a different screen.
const mapMaxHashDistance = 6

// learnBudget bounds the best-effort post-resolution learning capture so
// recording an element can't meaningfully extend the action.
const learnBudget = 2 * time.Second

// defaultMapBundle groups elements learned on a target with no launched
// bundle recorded (mirrors the template library's default).
const defaultMapBundle = "_global"

// captureClassedFunc captures the current screen, reporting which tool
// produced it — the "<backend>" half of the device-class key.
type captureClassedFunc func(ctx context.Context, udid string) (png []byte, backend string, err error)

// elementMapper wires one backend's element-map learning and fast-path
// resolution: the store, the backend's screen capture, and its
// launched-bundle lookup. A nil mapper (store not configured, --element-map
// off) disables both sides.
type elementMapper struct {
	store   *elementmap.Store
	capture captureClassedFunc
	bundle  func(udid string) string
}

func (em *elementMapper) enabled() bool { return em != nil && em.store != nil }

func (em *elementMapper) bundleFor(udid string) string {
	if em.bundle != nil {
		if b := em.bundle(udid); b != "" {
			return b
		}
	}
	return defaultMapBundle
}

// classify captures the current screen and derives its device-class key,
// returning the decoded image for hashing.
func (em *elementMapper) classify(ctx context.Context, udid string) (image.Image, string, error) {
	png, backend, err := em.capture(ctx, udid)
	if err != nil {
		return nil, "", err
	}
	img, _, err := image.Decode(bytes.NewReader(png))
	if err != nil {
		return nil, "", err
	}
	return img, fmt.Sprintf("%s/%d", backend, img.Bounds().Dx()), nil
}

// learn records one successfully-resolved element into the map:
// best-effort and time-bounded — a capture failure just skips the
// recording, never fails the action.
func (em *elementMapper) learn(ctx context.Context, udid, name string, n *Node) {
	if !em.enabled() || name == "" || n == nil || n.Frame == nil || n.Frame.W <= 0 || n.Frame.H <= 0 {
		return
	}
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), learnBudget)
	defer cancel()
	img, class, err := em.classify(lctx, udid)
	if err != nil {
		return
	}
	_ = em.store.Put(em.bundleFor(udid), class, name, elementmap.Element{
		Frame: elementmap.Frame{X: n.Frame.X, Y: n.Frame.Y, W: n.Frame.W, H: n.Frame.H},
		Role:  n.Role, Label: n.Label,
		ScreenFingerprint: elementmap.FormatFingerprint(imgutil.DHash(img, nil)),
	})
}

// invalidate drops the named element across all device classes for the
// target's bundle: a tap steered by the cached frame failed, so the
// entry can no longer be trusted.
func (em *elementMapper) invalidate(udid, name string) {
	if !em.enabled() || name == "" {
		return
	}
	_, _ = em.store.Remove(em.bundleFor(udid), name, "")
}

// elementMapLearner is implemented by an elementDriver whose composites
// learn/invalidate element-map entries (tap_element, type_into_element).
type elementMapLearner interface {
	learnElementMap(ctx context.Context, udid, name string, n *Node)
	invalidateElementMap(udid, name string)
}

// mapKey is the element map's key for one element spec: the matcher's
// canonical string, so the same payload always hits the same entry.
func mapKey(p map[string]any) string {
	m, err := matcherFromPayload(p)
	if err != nil {
		return ""
	}
	return m.String()
}

// mapResolver is the fast-path rung: a fingerprint-gated cache hit
// instead of an observe round-trip. A ladder is built per resolution,
// so missed memoizes one fingerprint mismatch for the rest of that
// resolution's polls — a screen that already missed cannot become a
// hit without an observing rung re-learning the entry first, so the
// miss path costs at most one screenshot per element action.
type mapResolver struct {
	em     *elementMapper
	missed bool
}

func (r *mapResolver) name() string          { return resolverMapName }
func (r *mapResolver) budget() time.Duration { return mapRungBudget }

func (r *mapResolver) resolve(ctx context.Context, udid string, m elementMatcher, refresh bool) (*Node, observation, error) {
	if refresh {
		// refresh:true asks for a cold, uncached observation on every
		// poll — a remembered frame is exactly what the caller is
		// opting out of.
		return nil, observation{}, errNoMatch
	}
	name := m.String()
	bundle := r.em.bundleFor(udid)
	if r.missed {
		return nil, observation{}, errNoMatch
	}
	// Pre-capture check: a bundle that never learned this element must
	// not pay for a screenshot. Nothing learns an entry mid-resolution,
	// so the negative lookup holds for the rest of this resolution's
	// polls too.
	if !r.em.store.Has(bundle, name) {
		r.missed = true
		return nil, observation{}, errNoMatch
	}
	img, class, err := r.em.classify(ctx, udid)
	if err != nil {
		// A capture failure hands over to the next rung; memoize it so
		// the miss path still costs at most one capture per action.
		r.missed = true
		return nil, observation{}, errNotYet
	}
	el, ok, err := r.em.store.Get(bundle, class, name)
	if err != nil || !ok {
		r.missed = true
		return nil, observation{}, errNoMatch
	}
	recorded, err := elementmap.ParseFingerprint(el.ScreenFingerprint)
	if err != nil {
		r.missed = true
		return nil, observation{}, errNoMatch
	}
	if imgutil.HashDistance(imgutil.DHash(img, nil), recorded) > mapMaxHashDistance {
		// The screen has drifted from the one the element was learned
		// on: fall through to the observing rungs (which re-learn).
		r.missed = true
		return nil, observation{}, errNoMatch
	}
	node := &Node{
		Role: el.Role, Label: el.Label, Interactable: true,
		Frame: &Frame{X: el.Frame.X, Y: el.Frame.Y, W: el.Frame.W, H: el.Frame.H},
	}
	// No observation: the whole point is skipping the observe; the
	// nil viewport skips findElement's on-screen check (the recorded
	// frame was tappable when learned, and a failed tap invalidates).
	return node, observation{}, nil
}
