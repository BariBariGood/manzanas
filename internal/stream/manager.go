// Package stream implements the media streamer: per-target MJPEG capture
// fanned out to N concurrent viewers over HTTP (multipart/x-mixed-replace)
// and WebSocket (binary JPEG frames). See docs/streaming.md.
package stream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/BariBariGood/manzanas/proto"
)

// ErrStreamLimit is returned by Open when MaxStreams is reached.
var ErrStreamLimit = errors.New("stream limit reached")

// ErrUnsupportedFormat is returned by Open for formats other than mjpeg.
var ErrUnsupportedFormat = errors.New("unsupported stream format")

// ErrBadStreamParam is returned by Open for out-of-range max_dim/quality.
var ErrBadStreamParam = errors.New("invalid stream parameter")

// FormatMJPEG is the only format implemented in v0.1.
const FormatMJPEG = "mjpeg"

// Manager implements the stream.Streamer contract: it negotiates streams
// and owns the per-target capture hubs. One stream (hub) exists per target;
// Open is idempotent per UDID and viewers of the same target share it.
type Manager struct {
	cfg     Config
	factory SourceFactory
	log     *slog.Logger

	mu     sync.Mutex
	byID   map[string]*hub
	byUDID map[string]*hub
}

// NewManager creates a Manager capturing via factory under cfg's limits.
func NewManager(cfg Config, factory SourceFactory, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		cfg:     cfg.withDefaults(),
		factory: factory,
		log:     log,
		byID:    make(map[string]*hub),
		byUDID:  make(map[string]*hub),
	}
}

func newStreamID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "stm_" + hex.EncodeToString(b)
}

// Open negotiates a stream for udid. The stream itself starts capturing
// only when its first viewer attaches (and lingers Config.Linger after the
// last one detaches). Format must be "mjpeg" (empty defaults to it).
func (m *Manager) Open(ctx context.Context, udid string, req proto.StreamRequest) (proto.StreamOffer, error) {
	format := req.Format
	if format == "" {
		format = FormatMJPEG
	}
	if format != FormatMJPEG {
		return proto.StreamOffer{}, fmt.Errorf("%w: %q (v0.1 serves mjpeg only)", ErrUnsupportedFormat, req.Format)
	}

	if req.Quality < 0 || req.Quality > 100 {
		return proto.StreamOffer{}, fmt.Errorf("%w: quality must be in 1..100", ErrBadStreamParam)
	}
	if req.MaxDim < 0 {
		return proto.StreamOffer{}, fmt.Errorf("%w: max_dim must be positive", ErrBadStreamParam)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.byUDID[udid]
	if !ok {
		if len(m.byID) >= m.cfg.MaxStreams {
			return proto.StreamOffer{}, ErrStreamLimit
		}
		// Opening is idempotent per target, so the first request's frame
		// transform (max_dim/quality) wins for the hub's lifetime; later
		// viewers can shrink further with per-attach query params.
		h = newHub(newStreamID(), udid, m.cfg.clampFPS(req.MaxFPS),
			req.MaxDim, req.Quality,
			m.cfg.Linger, m.cfg.MaxViewers, m.factory, m.log)
		h.onIdle = func(gen uint64) { m.reap(h, gen) }
		m.byID[h.id] = h
		m.byUDID[udid] = h
	}
	// (Re)arm the idle timer so a negotiated-but-not-yet-viewed stream gets
	// a full linger window before it is reaped — including reused hubs whose
	// previous window was about to expire.
	h.mu.Lock()
	if len(h.viewers) == 0 {
		h.armLingerLocked()
	}
	h.mu.Unlock()
	return proto.StreamOffer{
		StreamID: h.id,
		Format:   FormatMJPEG,
		URL:      "/v0/streams/" + h.id + "/ws",
		MJPEGURL: "/v0/streams/" + h.id + "/mjpeg",
		ViewURL:  "/view/" + udid,
		FPS:      h.fps,
		MaxDim:   h.maxDim,
		Quality:  h.quality,
	}, nil
}

// Close tears down a stream: all viewers are disconnected and the capture
// pump stops immediately.
func (m *Manager) Close(ctx context.Context, streamID string) error {
	m.mu.Lock()
	h, ok := m.byID[streamID]
	if ok {
		delete(m.byID, streamID)
		delete(m.byUDID, h.udid)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("stream %s not found", streamID)
	}
	h.close()
	return nil
}

// CloseAll tears down every open stream (daemon shutdown).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	hubs := make([]*hub, 0, len(m.byID))
	for _, h := range m.byID {
		hubs = append(hubs, h)
	}
	m.byID = make(map[string]*hub)
	m.byUDID = make(map[string]*hub)
	m.mu.Unlock()
	for _, h := range hubs {
		h.close()
	}
}

// reap unregisters a hub that has gone idle (linger elapsed with no
// viewers) so it stops counting against MaxStreams. gen is the idle
// generation the timer observed: closeIfStillIdle abandons the reap if a
// concurrent Open or attach re-armed the hub after the timer fired, and
// otherwise marks the hub closed so a racing attach (viewer that already
// resolved the hub) gets ErrStreamClosed instead of reviving it.
func (m *Manager) reap(h *hub, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byID[h.id] == h && h.closeIfStillIdle(gen) {
		delete(m.byID, h.id)
		delete(m.byUDID, h.udid)
	}
}

// get looks a hub up by stream ID.
func (m *Manager) get(streamID string) (*hub, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.byID[streamID]
	return h, ok
}

// HasStream reports whether a stream is currently open for the target.
// The warm pool consults it so an idle re-park never SIGSTOPs a tree a
// viewer is capturing frames from.
func (m *Manager) HasStream(udid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.byUDID[udid]
	return ok
}

// ViewerCount reports the number of viewers attached to a stream.
func (m *Manager) ViewerCount(streamID string) int {
	if h, ok := m.get(streamID); ok {
		return h.viewerCount()
	}
	return 0
}
