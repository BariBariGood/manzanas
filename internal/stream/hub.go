package stream

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/internal/imgutil"
)

// ErrViewerLimit is returned by attach when the stream is at MaxViewers.
var ErrViewerLimit = errors.New("stream viewer limit reached")

// ErrStreamClosed is returned by attach after the stream is torn down.
var ErrStreamClosed = errors.New("stream is closed")

// maxCaptureErrors is how many consecutive capture failures the pump
// tolerates (per-frame simctl invocations fail transiently) before it dies;
// a dead pump closes its viewers' channels so they can reconnect, which
// starts a fresh pump.
const maxCaptureErrors = 5

// defaultStallTimeout is how long a viewer may go without accepting a
// single frame before broadcast drops it. Slow viewers merely lose frames
// (their buffered channel is bypassed), but a viewer that stops draining
// entirely would otherwise pin one of the stream's MaxViewers slots until
// TCP keepalive reaps the connection.
const defaultStallTimeout = 30 * time.Second

// hub owns one target's capture pump and fans frames out to N viewers.
// The pump starts on the first viewer attach and stops after the last
// viewer detaches plus the configured linger.
type hub struct {
	id   string
	udid string
	fps  int
	// maxDim/quality, when set, downscale and JPEG re-encode each captured
	// frame once before fan-out (viewers can shrink further per-attach).
	maxDim     int
	quality    int
	linger     time.Duration
	maxViewers int
	factory    SourceFactory
	log        *slog.Logger
	// onIdle is called (outside the lock) when the linger elapses with
	// no viewers, so the owner can unregister the hub. It receives the
	// idle generation observed when the timer fired; the owner must
	// re-check it (idleGeneration) before unregistering, since a
	// concurrent Open may have re-armed the hub in the meantime.
	onIdle func(gen uint64)

	stall time.Duration

	mu       sync.Mutex
	idleGen  uint64                    // bumped by armLingerLocked and attach
	viewers  map[chan []byte]time.Time // channel -> last accepted frame
	pumpStop context.CancelFunc        // non-nil while the pump runs
	pumpDone chan struct{}
	lingerT  *time.Timer
	closed   bool
}

func newHub(id, udid string, fps, maxDim, quality int, linger time.Duration,
	maxViewers int, factory SourceFactory, log *slog.Logger) *hub {
	return &hub{
		id: id, udid: udid, fps: fps, maxDim: maxDim, quality: quality,
		linger:     linger,
		maxViewers: maxViewers, factory: factory, log: log,
		stall:   defaultStallTimeout,
		viewers: make(map[chan []byte]time.Time),
	}
}

// attach registers a viewer and returns its frame channel plus a detach
// func. Starting the pump (if idle) and cancelling any pending linger stop
// happen atomically with registration.
func (h *hub) attach() (<-chan []byte, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, ErrStreamClosed
	}
	if len(h.viewers) >= h.maxViewers {
		return nil, nil, ErrViewerLimit
	}
	ch := make(chan []byte, 4)
	h.viewers[ch] = time.Now()
	h.idleGen++ // invalidate any in-flight idle reap
	if h.lingerT != nil {
		h.lingerT.Stop()
		h.lingerT = nil
	}
	if h.pumpStop == nil {
		h.startPumpLocked()
	}
	var once sync.Once
	detach := func() { once.Do(func() { h.detach(ch) }) }
	return ch, detach, nil
}

func (h *hub) detach(ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.viewers, ch)
	if len(h.viewers) == 0 {
		h.armLingerLocked()
	}
}

// armLingerLocked (re)schedules the idle timer: when it fires with no
// viewers, the pump stops (if still running) and onIdle lets the owner
// unregister the hub. Armed whenever the hub becomes (or is created)
// viewer-less, regardless of pump state, so dead-pump and never-viewed
// hubs are reaped too.
func (h *hub) armLingerLocked() {
	if h.closed {
		return
	}
	if h.lingerT != nil {
		h.lingerT.Stop()
	}
	h.idleGen++
	gen := h.idleGen
	h.lingerT = time.AfterFunc(h.linger, func() {
		h.mu.Lock()
		idle := gen == h.idleGen && len(h.viewers) == 0 && !h.closed
		if idle {
			h.stopPumpLocked()
		}
		onIdle := h.onIdle
		h.mu.Unlock()
		if idle && onIdle != nil {
			onIdle(gen)
		}
	})
}

// idleGeneration reports the current idle generation; a reaper compares it
// with the generation its timer observed to detect a concurrent re-arm.
func (h *hub) idleGeneration() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.idleGen
}

// closeIfStillIdle atomically re-checks that the hub is still idle at the
// generation the reaper's timer observed and, if so, marks it closed so a
// racing attach (viewer that already resolved the hub) fails with
// ErrStreamClosed instead of reviving an unregistered hub. Returns whether
// the hub was closed (and may therefore be unregistered).
func (h *hub) closeIfStillIdle(gen uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || gen != h.idleGen || len(h.viewers) != 0 {
		return false
	}
	h.closed = true
	if h.lingerT != nil {
		h.lingerT.Stop()
		h.lingerT = nil
	}
	h.stopPumpLocked()
	return true
}

// viewerCount reports the number of attached viewers.
func (h *hub) viewerCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.viewers)
}

// close tears the stream down: all viewer channels close and the pump stops.
func (h *hub) close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	if h.lingerT != nil {
		h.lingerT.Stop()
		h.lingerT = nil
	}
	for ch := range h.viewers {
		close(ch)
		delete(h.viewers, ch)
	}
	done := h.pumpDone
	h.stopPumpLocked()
	h.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (h *hub) startPumpLocked() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	h.pumpStop = cancel
	h.pumpDone = done
	go h.pump(ctx, done)
}

func (h *hub) stopPumpLocked() {
	if h.pumpStop != nil {
		h.pumpStop()
		h.pumpStop = nil
		h.pumpDone = nil
	}
}

// pump captures frames at the configured rate and broadcasts them. Slow
// viewers drop frames rather than stalling capture or their peers. On exit
// it settles the hub's pump state and, unless the stop was deliberate,
// disconnects viewers so their handlers end instead of hanging on a silent
// channel (reconnecting attaches start a fresh pump).
func (h *hub) pump(ctx context.Context, done chan struct{}) {
	defer close(done)
	defer h.settlePump(ctx, done)

	src, err := h.factory(h.udid)
	if err != nil {
		h.log.Error("stream source open failed", "stream", h.id, "udid", h.udid, "err", err)
		return
	}
	defer src.Close()

	interval := time.Second / time.Duration(h.fps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	errs := 0
	for {
		frame, err := src.Next(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return
		case err != nil:
			errs++
			h.log.Warn("stream capture failed", "stream", h.id, "udid", h.udid,
				"consecutive", errs, "err", err)
			if errs >= maxCaptureErrors {
				return
			}
		default:
			errs = 0
			h.broadcast(h.transform(frame))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// settlePump clears the hub's pump bookkeeping when the pump goroutine
// exits on its own (source failure). Without this, attach would see a
// running pump forever and viewers would hang on a channel that never
// closes. Deliberate stops (stopPumpLocked/close) already cleared the
// state, recognizable because pumpDone no longer points at this pump.
func (h *hub) settlePump(ctx context.Context, done chan struct{}) {
	if ctx.Err() != nil {
		return // cancelled by stopPumpLocked or close
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pumpDone != done {
		return
	}
	h.pumpStop = nil
	h.pumpDone = nil
	for ch := range h.viewers {
		close(ch)
		delete(h.viewers, ch)
	}
	h.armLingerLocked()
}

// transform applies the hub's configured downscale/re-encode to one
// captured frame. Best-effort: an undecodable frame passes through
// untouched rather than dropping it.
func (h *hub) transform(frame []byte) []byte {
	if h.maxDim <= 0 && h.quality <= 0 {
		return frame
	}
	out, err := imgutil.Transcode(frame, "jpeg", h.quality, h.maxDim)
	if err != nil {
		h.log.Warn("stream frame transform failed", "stream", h.id, "udid", h.udid, "err", err)
		return frame
	}
	return out
}

// broadcast fans a frame out to every viewer, pacing each to the rate it
// actually accepts: a viewer whose buffer is full has its oldest queued
// frame replaced by this newest one (latest-frame-wins), so a slow client
// never watches a stale backlog (head-of-line blocking) and never stalls
// capture or its peers. A viewer that has not accepted any frame for the
// stall timeout is disconnected so it stops occupying a MaxViewers slot.
func (h *hub) broadcast(frame []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	dropped := false
	for ch, last := range h.viewers {
		select {
		case ch <- frame:
			h.viewers[ch] = now
			continue
		default:
		}
		// Buffer full: evict the oldest queued frame and offer the newest.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- frame:
		default:
		}
		if now.Sub(last) > h.stall {
			h.log.Warn("dropping stalled stream viewer", "stream", h.id, "udid", h.udid)
			close(ch)
			delete(h.viewers, ch)
			dropped = true
		}
	}
	if dropped && len(h.viewers) == 0 {
		h.armLingerLocked()
	}
}
