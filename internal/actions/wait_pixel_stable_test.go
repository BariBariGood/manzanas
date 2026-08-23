package actions

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"sync"
	"testing"
)

// frame renders a white 80x160 image with an optional black square whose
// top-left corner is at (x, y).
func frame(t *testing.T, drawBox bool, x, y int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 80, 160))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	if drawBox {
		for dy := 0; dy < 32; dy++ {
			for dx := 0; dx < 32; dx++ {
				img.Set(x+dx, y+dy, color.Black)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	return buf.Bytes()
}

// seqCapture replays a fixed sequence of frames; the last repeats once
// the sequence is exhausted.
type seqCapture struct {
	mu     sync.Mutex
	frames [][]byte
	calls  int
}

func (s *seqCapture) capture(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	if i >= len(s.frames) {
		i = len(s.frames) - 1
	}
	s.calls++
	return s.frames[i], nil
}

func fastPixelWait(extra map[string]any) map[string]any {
	p := map[string]any{"timeout_ms": 500.0, "interval_ms": 10.0, "quiet_ms": 20.0}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func TestWaitPixelStableSettles(t *testing.T) {
	// The box moves twice, then the screen holds still.
	s := &seqCapture{frames: [][]byte{
		frame(t, true, 0, 0),
		frame(t, true, 20, 40),
		frame(t, true, 40, 80),
		frame(t, true, 40, 80),
	}}
	res, err := pixelStable(context.Background(), fastPixelWait(nil), s.capture)
	if err != nil {
		t.Fatalf("wait_for_stable: %v", err)
	}
	if res["stable"] != true {
		t.Fatalf("expected stable:true, got %+v", res)
	}
	if d := res["last_diff"].(float64); d >= defaultPixelThreshold {
		t.Fatalf("last_diff = %v, want < threshold", d)
	}
	if res["samples"].(int) < 4 {
		t.Fatalf("samples = %v, want >= 4", res["samples"])
	}
	if _, ok := res["hash"]; ok {
		t.Fatalf("result must not carry a 'hash' key (journal AXAfter lift): %+v", res)
	}
}

func TestWaitPixelStableNeverSettles(t *testing.T) {
	// The box alternates position forever: every comparison is a change.
	s := &seqCapture{}
	pos := 0
	capture := func(ctx context.Context) ([]byte, error) {
		pos = (pos + 1) % 2
		return frame(t, true, pos*40, pos*80), nil
	}
	_ = s
	res, err := pixelStable(context.Background(),
		map[string]any{"timeout_ms": 100.0, "interval_ms": 10.0, "quiet_ms": 20.0}, capture)
	if err != nil {
		t.Fatalf("wait_for_stable: %v", err)
	}
	if res["stable"] != false {
		t.Fatalf("expected stable:false, got %+v", res)
	}
	if d := res["last_diff"].(float64); d <= 0 {
		t.Fatalf("last_diff = %v, want > 0", d)
	}
	if res["waited_ms"].(int64) < 90 {
		t.Fatalf("waited_ms = %v, want ~ the timeout", res["waited_ms"])
	}
}

func TestWaitPixelStableRequireStable(t *testing.T) {
	pos := 0
	capture := func(ctx context.Context) ([]byte, error) {
		pos = (pos + 1) % 2
		return frame(t, true, pos*40, pos*80), nil
	}
	_, err := pixelStable(context.Background(),
		map[string]any{"timeout_ms": 100.0, "interval_ms": 10.0, "quiet_ms": 20.0,
			"require_stable": true}, capture)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "timeout" {
		t.Fatalf("want timeout error, got %v", err)
	}
}

func TestWaitPixelStableMaskHidesVolatileRegion(t *testing.T) {
	// The box keeps moving inside the top-left quadrant, but a mask
	// covers that whole quadrant, so the screen counts as settled.
	pos := 0
	capture := func(ctx context.Context) ([]byte, error) {
		pos = (pos + 1) % 2
		return frame(t, true, pos*8, pos*16), nil
	}
	res, err := pixelStable(context.Background(),
		fastPixelWait(map[string]any{"mask": []any{
			map[string]any{"x": 0.0, "y": 0.0, "w": 48.0, "h": 64.0},
		}}), capture)
	if err != nil {
		t.Fatalf("wait_for_stable with mask: %v", err)
	}
	if res["stable"] != true {
		t.Fatalf("expected stable:true under mask, got %+v", res)
	}
}

func TestWaitPixelStableThresholdAdmitsSmallChange(t *testing.T) {
	// A single 2x2 changing dot on an 80x160 screen is below a loose
	// threshold, so the wait settles despite the change.
	pos := 0
	capture := func(ctx context.Context) ([]byte, error) {
		pos = (pos + 1) % 2
		img := image.NewRGBA(image.Rect(0, 0, 80, 160))
		for i := range img.Pix {
			img.Pix[i] = 0xff
		}
		img.Set(pos, 0, color.Black)
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	res, err := pixelStable(context.Background(),
		fastPixelWait(map[string]any{"threshold": 0.05}), capture)
	if err != nil {
		t.Fatalf("wait_for_stable: %v", err)
	}
	if res["stable"] != true {
		t.Fatalf("expected stable:true under loose threshold, got %+v", res)
	}
}

func TestWaitPixelStableBadPayload(t *testing.T) {
	capture := func(context.Context) ([]byte, error) { return frame(t, false, 0, 0), nil }
	cases := []map[string]any{
		{"threshold": 0.0},
		{"threshold": 1.0},
		{"threshold": "high"},
		{"quiet_ms": -5.0},
		{"mask": "nope"},
		{"mask": []any{map[string]any{"x": 0.0, "y": 0.0, "w": 0.0, "h": 10.0}}},
		{"mask": []any{map[string]any{"x": 0.0, "y": 0.0, "w": 10.0}}},
		{"require_stable": "yes"},
	}
	for _, p := range cases {
		_, err := pixelStable(context.Background(), p, capture)
		var ae *Error
		if !errors.As(err, &ae) || ae.Code != "bad_request" {
			t.Fatalf("payload %v: want bad_request, got %v", p, err)
		}
	}
}

func TestWaitPixelStableUndecodableCapture(t *testing.T) {
	capture := func(context.Context) ([]byte, error) { return []byte("not an image"), nil }
	_, err := pixelStable(context.Background(), fastPixelWait(nil), capture)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "internal" {
		t.Fatalf("want internal error, got %v", err)
	}
}

// The simulator handler must route through the backend screenshot path
// (AXe screenshot --output), settling on the fake runner's constant frame.
func TestWaitPixelStableViaAXeBackend(t *testing.T) {
	f := newFakeRunner()
	f.writeFile["screenshot"] = string(frame(t, true, 8, 8))
	res, err := dispatch(t, testBackend(f), "wait_for_stable",
		fastPixelWait(nil))
	if err != nil {
		t.Fatalf("wait_for_stable via backend: %v", err)
	}
	if res.Result["stable"] != true {
		t.Fatalf("expected stable:true, got %+v", res.Result)
	}
}

func TestWaitPixelStableNoFramesIsError(t *testing.T) {
	// Every capture fails transiently: the budget expires without a
	// single frame comparison, which is a poll failure, not a soft
	// stable:false observation.
	cap := func(context.Context) ([]byte, error) { return nil, errNotYet }
	_, err := pixelStable(context.Background(), fastPixelWait(nil), cap)
	if err == nil {
		t.Fatal("expected error when no frames were ever captured")
	}
}

func TestWaitPixelStableCaptureBlipResetsQuietStreak(t *testing.T) {
	// A transient capture failure mid-streak restarts the quiet window:
	// the unobserved gap (and the stale pre-outage frame) must not count.
	still := frame(t, true, 8, 8)
	var mu sync.Mutex
	calls := 0
	cap := func(context.Context) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 3 {
			return nil, errNotYet
		}
		return still, nil
	}
	res, err := pixelStable(context.Background(), fastPixelWait(map[string]any{"quiet_ms": 30.0}), cap)
	if err != nil {
		t.Fatalf("wait_for_stable: %v", err)
	}
	if res["stable"] != true {
		t.Fatalf("expected stable:true, got %+v", res)
	}
	// quiet 30ms / interval 10ms = 3 consecutive comparisons. Without the
	// blip the wait settles in 4 captures; the blip at call 3 wipes the
	// streak and the baseline, so settling needs a fresh baseline (call 4)
	// plus three comparisons: 7 captures.
	if s := res["samples"].(int); s < 7 {
		t.Fatalf("samples = %d, want >= 7 (streak must restart after the blip)", s)
	}
}
