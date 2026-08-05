package stream

import (
	"log/slog"
	"testing"
	"time"
)

// TestBroadcastLatestFrameWins: a viewer that stops draining keeps getting
// the newest frames (oldest queued frames are evicted) instead of a stale
// backlog, and capture is never blocked.
func TestBroadcastLatestFrameWins(t *testing.T) {
	h := newHub("stm_test", "UDID-1", 10, 0, 0, time.Minute, 4, FakeSourceFactory, slog.Default())
	ch := make(chan []byte, 4)
	h.viewers[ch] = time.Now()

	for i := byte(1); i <= 6; i++ {
		h.broadcast([]byte{i})
	}
	// Buffer cap is 4; frames 1 and 2 must have been evicted for 5 and 6.
	want := []byte{3, 4, 5, 6}
	for _, w := range want {
		select {
		case f := <-ch:
			if len(f) != 1 || f[0] != w {
				t.Fatalf("got frame %v, want [%d]", f, w)
			}
		default:
			t.Fatalf("channel empty, want frame [%d]", w)
		}
	}
	select {
	case f := <-ch:
		t.Fatalf("unexpected extra frame %v", f)
	default:
	}
}

// TestBroadcastDropsStalledViewer: a viewer that accepts nothing past the
// stall timeout is disconnected so it stops pinning a viewer slot.
func TestBroadcastDropsStalledViewer(t *testing.T) {
	h := newHub("stm_test", "UDID-1", 10, 0, 0, time.Minute, 4, FakeSourceFactory, slog.Default())
	h.stall = time.Millisecond
	ch := make(chan []byte, 4)
	for i := 0; i < 4; i++ {
		ch <- []byte{0} // full buffer: every broadcast takes the evict path
	}
	h.viewers[ch] = time.Now().Add(-time.Second)

	h.broadcast([]byte{1})
	h.mu.Lock()
	_, present := h.viewers[ch]
	h.mu.Unlock()
	if present {
		t.Fatal("stalled viewer still registered")
	}
}
