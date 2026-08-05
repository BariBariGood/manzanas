package stream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

func testManager(t *testing.T, cfg Config) *Manager {
	t.Helper()
	m := NewManager(cfg, FakeSourceFactory, nil)
	t.Cleanup(m.CloseAll)
	return m
}

func TestOpenDefaultsToMJPEG(t *testing.T) {
	m := testManager(t, Config{})
	offer, err := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if offer.Format != FormatMJPEG {
		t.Errorf("format = %q, want %q", offer.Format, FormatMJPEG)
	}
	if offer.StreamID == "" || offer.URL == "" || offer.MJPEGURL == "" || offer.ViewURL == "" {
		t.Errorf("offer has empty fields: %+v", offer)
	}
}

func TestOpenRejectsUnsupportedFormat(t *testing.T) {
	m := testManager(t, Config{})
	_, err := m.Open(context.Background(), "UDID-1", proto.StreamRequest{Format: "h264"})
	if err == nil {
		t.Fatal("want error for h264, got nil")
	}
}

func TestOpenIsIdempotentPerTarget(t *testing.T) {
	m := testManager(t, Config{})
	a, err := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b, err := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if a.StreamID != b.StreamID {
		t.Errorf("stream IDs differ for same target: %q vs %q", a.StreamID, b.StreamID)
	}
	c, err := m.Open(context.Background(), "UDID-2", proto.StreamRequest{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if c.StreamID == a.StreamID {
		t.Error("different targets share a stream ID")
	}
}

func TestOpenEnforcesMaxStreams(t *testing.T) {
	m := testManager(t, Config{MaxStreams: 1})
	if _, err := m.Open(context.Background(), "UDID-1", proto.StreamRequest{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := m.Open(context.Background(), "UDID-2", proto.StreamRequest{}); err != ErrStreamLimit {
		t.Fatalf("err = %v, want ErrStreamLimit", err)
	}
	// Closing frees capacity.
	offer, _ := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	if err := m.Close(context.Background(), offer.StreamID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := m.Open(context.Background(), "UDID-2", proto.StreamRequest{}); err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
}

func TestCloseUnknownStream(t *testing.T) {
	m := testManager(t, Config{})
	if err := m.Close(context.Background(), "stm_nope"); err == nil {
		t.Fatal("want error closing unknown stream")
	}
}

func mustFrame(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatal("frame channel closed")
		}
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for frame")
		return nil
	}
}

func TestMultiViewerReceivesFrames(t *testing.T) {
	m := testManager(t, Config{DefaultFPS: 30})
	offer, err := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	h, _ := m.get(offer.StreamID)

	ch1, detach1, err := h.attach()
	if err != nil {
		t.Fatalf("attach 1: %v", err)
	}
	defer detach1()
	ch2, detach2, err := h.attach()
	if err != nil {
		t.Fatalf("attach 2: %v", err)
	}
	defer detach2()

	f1 := mustFrame(t, ch1)
	f2 := mustFrame(t, ch2)
	if len(f1) == 0 || len(f2) == 0 {
		t.Fatal("empty frames")
	}
	// JPEG SOI marker.
	if f1[0] != 0xFF || f1[1] != 0xD8 {
		t.Errorf("frame is not JPEG: % x", f1[:2])
	}

	// A viewer leaving must not affect the other.
	detach1()
	mustFrame(t, ch2)
}

func TestViewerLimit(t *testing.T) {
	m := testManager(t, Config{MaxViewers: 1})
	offer, _ := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	h, _ := m.get(offer.StreamID)
	_, detach, err := h.attach()
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	if _, _, err := h.attach(); err != ErrViewerLimit {
		t.Fatalf("err = %v, want ErrViewerLimit", err)
	}
}

func TestPumpStartsOnFirstViewerAndStopsAfterLinger(t *testing.T) {
	m := testManager(t, Config{DefaultFPS: 30, Linger: 50 * time.Millisecond})
	offer, _ := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	h, _ := m.get(offer.StreamID)

	h.mu.Lock()
	running := h.pumpStop != nil
	h.mu.Unlock()
	if running {
		t.Fatal("pump running before first viewer")
	}

	ch, detach, err := h.attach()
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	mustFrame(t, ch)
	h.mu.Lock()
	running = h.pumpStop != nil
	h.mu.Unlock()
	if !running {
		t.Fatal("pump not running with a viewer attached")
	}

	detach()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		running = h.pumpStop != nil
		h.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pump still running after linger elapsed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLingerCancelledByNewViewer(t *testing.T) {
	m := testManager(t, Config{DefaultFPS: 30, Linger: time.Hour})
	offer, _ := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	h, _ := m.get(offer.StreamID)

	ch1, detach1, _ := h.attach()
	mustFrame(t, ch1)
	detach1()

	ch2, detach2, err := h.attach()
	if err != nil {
		t.Fatalf("attach during linger: %v", err)
	}
	defer detach2()
	mustFrame(t, ch2)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lingerT != nil {
		t.Error("linger timer still pending after new viewer attached")
	}
}

func TestCloseDisconnectsViewers(t *testing.T) {
	m := testManager(t, Config{DefaultFPS: 30})
	offer, _ := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	h, _ := m.get(offer.StreamID)
	ch, detach, _ := h.attach()
	defer detach()
	mustFrame(t, ch)

	if err := m.Close(context.Background(), offer.StreamID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // channel closed as expected
			}
		case <-deadline:
			t.Fatal("viewer channel not closed after stream Close")
		}
	}
}

// failingSource errors on every Next, simulating persistent capture failure.
type failingSource struct{}

func (failingSource) Next(context.Context) ([]byte, error) {
	return nil, context.DeadlineExceeded
}
func (failingSource) Close() error { return nil }

func TestPumpDeathDisconnectsViewersAndAllowsRestart(t *testing.T) {
	m := NewManager(Config{DefaultFPS: 30}, func(string) (FrameSource, error) {
		return failingSource{}, nil
	}, nil)
	t.Cleanup(m.CloseAll)
	offer, _ := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	h, _ := m.get(offer.StreamID)

	ch, detach, err := h.attach()
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("got a frame from a failing source")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("viewer channel not closed after pump death")
	}

	// The dead pump's bookkeeping is cleared so a new viewer restarts it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		settled := h.pumpStop == nil
		h.mu.Unlock()
		if settled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pump state not settled after death")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, detach2, err := h.attach(); err != nil {
		t.Fatalf("re-attach after pump death: %v", err)
	} else {
		defer detach2()
	}
	h.mu.Lock()
	running := h.pumpStop != nil
	h.mu.Unlock()
	if !running {
		t.Fatal("pump not restarted by new viewer")
	}
}

func TestNeverViewedStreamIsReaped(t *testing.T) {
	m := testManager(t, Config{MaxStreams: 1, DefaultFPS: 30, Linger: 20 * time.Millisecond})
	offer, _ := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := m.get(offer.StreamID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never-viewed hub not reaped after linger")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := m.Open(context.Background(), "UDID-2", proto.StreamRequest{}); err != nil {
		t.Fatalf("Open after reap: %v", err)
	}
}

func TestIdleStreamsAreReaped(t *testing.T) {
	m := testManager(t, Config{MaxStreams: 1, DefaultFPS: 30, Linger: 20 * time.Millisecond})
	offer, _ := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	h, _ := m.get(offer.StreamID)

	// Attach+detach; the hub must be reaped after the linger, freeing capacity.
	ch, detach, err := h.attach()
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	mustFrame(t, ch)
	detach()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := m.get(offer.StreamID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle hub not reaped after linger")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := m.Open(context.Background(), "UDID-2", proto.StreamRequest{}); err != nil {
		t.Fatalf("Open after reap: %v", err)
	}
}

func TestReapAbandonedWhenReArmed(t *testing.T) {
	m := testManager(t, Config{DefaultFPS: 30, Linger: time.Hour})
	offer, _ := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	h, _ := m.get(offer.StreamID)

	// Simulate a reap whose timer fired just before a re-arm (Open/attach
	// bumped the generation): the stale-generation reap must be a no-op.
	stale := h.idleGeneration()
	h.mu.Lock()
	h.armLingerLocked()
	h.mu.Unlock()
	m.reap(h, stale)
	if _, ok := m.get(offer.StreamID); !ok {
		t.Fatal("hub reaped despite re-armed idle generation")
	}
	// A reap with the current generation still works, and it closes the
	// hub so a racing attach can't revive an unregistered stream.
	m.reap(h, h.idleGeneration())
	if _, ok := m.get(offer.StreamID); ok {
		t.Fatal("hub not reaped with matching generation")
	}
	if _, _, err := h.attach(); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("attach after reap: got %v, want ErrStreamClosed", err)
	}
}

func TestOfferEchoesEffectiveFPS(t *testing.T) {
	m := testManager(t, Config{DefaultFPS: 10, MaxFPS: 30})
	for _, tc := range []struct {
		udid string
		req  int
		want int
	}{
		{"UDID-1", 0, 10}, {"UDID-2", 100, 30}, {"UDID-3", -5, 10}, {"UDID-4", 15, 15},
	} {
		offer, err := m.Open(context.Background(), tc.udid, proto.StreamRequest{MaxFPS: tc.req})
		if err != nil {
			t.Fatalf("Open(max_fps=%d): %v", tc.req, err)
		}
		if offer.FPS != tc.want {
			t.Errorf("Open(max_fps=%d): offer.FPS = %d, want %d", tc.req, offer.FPS, tc.want)
		}
	}
}

func TestStalledViewerDropped(t *testing.T) {
	m := testManager(t, Config{DefaultFPS: 30, Linger: time.Hour})
	offer, _ := m.Open(context.Background(), "UDID-1", proto.StreamRequest{})
	h, _ := m.get(offer.StreamID)
	h.stall = 50 * time.Millisecond

	// stalled never reads; healthy keeps draining.
	stalled, detachStalled, err := h.attach()
	if err != nil {
		t.Fatalf("attach stalled: %v", err)
	}
	defer detachStalled()
	healthy, detachHealthy, err := h.attach()
	if err != nil {
		t.Fatalf("attach healthy: %v", err)
	}
	defer detachHealthy()

	// Never read from stalled; keep healthy drained until the hub drops
	// the stalled viewer.
	deadline := time.Now().Add(5 * time.Second)
	for h.viewerCount() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("stalled viewer not dropped")
		}
		select {
		case _, ok := <-healthy:
			if !ok {
				t.Fatal("healthy viewer channel closed")
			}
		case <-time.After(5 * time.Millisecond):
		}
	}
	mustFrame(t, healthy)
	// The dropped viewer's channel must be closed (after any buffered frames).
	for {
		select {
		case _, ok := <-stalled:
			if !ok {
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("stalled viewer channel not closed")
		}
	}
}

func TestClampFPS(t *testing.T) {
	cfg := Config{DefaultFPS: 10, MaxFPS: 30}.withDefaults()
	for _, tc := range []struct{ in, want int }{
		{0, 10}, {-5, 10}, {15, 15}, {99, 30},
	} {
		if got := cfg.clampFPS(tc.in); got != tc.want {
			t.Errorf("clampFPS(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
