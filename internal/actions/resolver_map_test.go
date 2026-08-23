package actions

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/BariBariGood/manzanas/internal/elementmap"
	"github.com/BariBariGood/manzanas/internal/imgutil"
)

// testScreen renders a small deterministic screen whose DHash differs
// sharply between variants.
func testScreen(t *testing.T, variant int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 100, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 100; x++ {
			v := uint8((x*variant + y*(variant+3)) % 256)
			img.Set(x, y, color.RGBA{v, 255 - v, v / 2, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testMapper(t *testing.T, screen *[]byte) *elementMapper {
	t.Helper()
	st, err := elementmap.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &elementMapper{
		store: st,
		capture: func(ctx context.Context, udid string) ([]byte, string, error) {
			return *screen, "test", nil
		},
		bundle: func(udid string) string { return "com.example.app" },
	}
}

func testMatcher(t *testing.T, label string) elementMatcher {
	t.Helper()
	m, err := matcherFromPayload(map[string]any{"label": label})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMapResolverHitAfterLearn(t *testing.T) {
	screen := testScreen(t, 1)
	em := testMapper(t, &screen)
	node := &Node{Role: "Button", Label: "Reset", Frame: &Frame{X: 10, Y: 20, W: 80, H: 30}}
	m := testMatcher(t, "Reset")
	em.learn(context.Background(), "sim-1", m.String(), node)

	r := &mapResolver{em: em}
	hit, obs, err := r.resolve(context.Background(), "sim-1", m, false)
	if err != nil {
		t.Fatalf("resolve after learn: %v", err)
	}
	if hit.Frame == nil || *hit.Frame != *node.Frame || hit.Label != "Reset" {
		t.Fatalf("bad cached node: %+v", hit)
	}
	if obs.nodes != nil || obs.viewport != nil {
		t.Fatalf("map rung produced an observation: %+v", obs)
	}
	if r.name() != "map" {
		t.Fatalf("rung name = %q", r.name())
	}
}

func TestMapResolverSkipsOnRefresh(t *testing.T) {
	screen := testScreen(t, 1)
	em := testMapper(t, &screen)
	m := testMatcher(t, "Reset")
	em.learn(context.Background(), "sim-1", m.String(),
		&Node{Role: "Button", Label: "Reset", Frame: &Frame{X: 10, Y: 20, W: 80, H: 30}})
	captured := false
	em.capture = func(ctx context.Context, udid string) ([]byte, string, error) {
		captured = true
		return screen, "test", nil
	}
	r := &mapResolver{em: em}
	if _, _, err := r.resolve(context.Background(), "sim-1", m, true); err != errNoMatch {
		t.Fatalf("resolve with refresh: %v, want errNoMatch", err)
	}
	if captured {
		t.Fatal("refresh resolve captured a screenshot")
	}
}

func TestMapResolverMissWhenScreenDrifts(t *testing.T) {
	screen := testScreen(t, 1)
	em := testMapper(t, &screen)
	m := testMatcher(t, "Reset")
	em.learn(context.Background(), "sim-1", m.String(),
		&Node{Role: "Button", Label: "Reset", Frame: &Frame{X: 10, Y: 20, W: 80, H: 30}})

	// Sanity: the two variants really are far apart in hash space.
	imgA, _, _ := image.Decode(bytes.NewReader(testScreen(t, 1)))
	imgB, _, _ := image.Decode(bytes.NewReader(testScreen(t, 9)))
	if d := imgutil.HashDistance(imgutil.DHash(imgA, nil), imgutil.DHash(imgB, nil)); d <= mapMaxHashDistance {
		t.Fatalf("test screens too similar (distance %d)", d)
	}

	screen = testScreen(t, 9)
	r := &mapResolver{em: em}
	if _, _, err := r.resolve(context.Background(), "sim-1", m, false); err != errNoMatch {
		t.Fatalf("resolve on drifted screen: %v, want errNoMatch", err)
	}

	// The miss is memoized: later polls of the same resolution must not
	// pay for another capture.
	captured := false
	em.capture = func(ctx context.Context, udid string) ([]byte, string, error) {
		captured = true
		return screen, "test", nil
	}
	if _, _, err := r.resolve(context.Background(), "sim-1", m, false); err != errNoMatch {
		t.Fatalf("second resolve after miss: %v, want errNoMatch", err)
	}
	if captured {
		t.Fatal("memoized miss captured another screenshot")
	}
}

func TestMapResolverMissWithoutEntry(t *testing.T) {
	screen := testScreen(t, 1)
	em := testMapper(t, &screen)
	captured := false
	em.capture = func(ctx context.Context, udid string) ([]byte, string, error) {
		captured = true
		return screen, "test", nil
	}
	r := &mapResolver{em: em}
	if _, _, err := r.resolve(context.Background(), "sim-1", testMatcher(t, "Missing"), false); err != errNoMatch {
		t.Fatalf("resolve without entry: %v, want errNoMatch", err)
	}
	if captured {
		t.Fatal("map rung captured a screenshot for an unknown element")
	}
}

func TestMapResolverCaptureFailureHandsOver(t *testing.T) {
	screen := testScreen(t, 1)
	em := testMapper(t, &screen)
	m := testMatcher(t, "Reset")
	em.learn(context.Background(), "sim-1", m.String(),
		&Node{Role: "Button", Label: "Reset", Frame: &Frame{X: 10, Y: 20, W: 80, H: 30}})
	em.capture = func(ctx context.Context, udid string) ([]byte, string, error) {
		return nil, "", fmt.Errorf("capture down")
	}
	r := &mapResolver{em: em}
	if _, _, err := r.resolve(context.Background(), "sim-1", m, false); err != errNotYet {
		t.Fatalf("resolve with broken capture: %v, want errNotYet", err)
	}
}

func TestMapperInvalidate(t *testing.T) {
	screen := testScreen(t, 1)
	em := testMapper(t, &screen)
	m := testMatcher(t, "Reset")
	em.learn(context.Background(), "sim-1", m.String(),
		&Node{Role: "Button", Label: "Reset", Frame: &Frame{X: 10, Y: 20, W: 80, H: 30}})
	if !em.store.Has("com.example.app", m.String()) {
		t.Fatal("learn did not record the element")
	}
	em.invalidate("sim-1", m.String())
	if em.store.Has("com.example.app", m.String()) {
		t.Fatal("invalidate left the entry behind")
	}
	r := &mapResolver{em: em}
	if _, _, err := r.resolve(context.Background(), "sim-1", m, false); err != errNoMatch {
		t.Fatalf("resolve after invalidate: %v, want errNoMatch", err)
	}
}

func TestMapperLearnSkipsUnusableNodes(t *testing.T) {
	screen := testScreen(t, 1)
	em := testMapper(t, &screen)
	em.learn(context.Background(), "sim-1", "n", nil)
	em.learn(context.Background(), "sim-1", "n", &Node{Label: "no frame"})
	em.learn(context.Background(), "sim-1", "", &Node{Frame: &Frame{W: 10, H: 10}})
	if list, _ := em.store.List(); len(list) != 0 {
		t.Fatalf("unusable nodes recorded: %+v", list)
	}
}

func TestNilMapperDisabled(t *testing.T) {
	var em *elementMapper
	if em.enabled() {
		t.Fatal("nil mapper enabled")
	}
	// learn/invalidate on a nil mapper must be safe no-ops.
	em.learn(context.Background(), "sim-1", "n", &Node{Frame: &Frame{W: 1, H: 1}})
	em.invalidate("sim-1", "n")
}
