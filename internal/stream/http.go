package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/BariBariGood/manzanas/internal/imgutil"
	"github.com/BariBariGood/manzanas/proto"
)

const mjpegBoundary = "manzanasdframe"

// frameWriteTimeout bounds each outbound media frame write (both
// transports), so a stalled client that stops reading is dropped instead
// of pinning its viewer slot and the capture pump forever.
const frameWriteTimeout = 10 * time.Second

// ServeMJPEG streams a stream's frames as multipart/x-mixed-replace JPEG
// parts until the client disconnects. Any HTTP client (browser <img>, curl)
// can consume it.
func (m *Manager) ServeMJPEG(w http.ResponseWriter, r *http.Request, streamID string) {
	h, ok := m.get(streamID)
	if !ok {
		writeWireError(w, http.StatusNotFound, proto.ErrNotFound, "stream "+streamID+" not found")
		return
	}
	transform, err := viewerTransform(r.URL.Query())
	if err != nil {
		writeWireError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	frames, detach, err := h.attach()
	if err != nil {
		writeAttachError(w, err)
		return
	}
	defer detach()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeWireError(w, http.StatusInternalServerError, proto.ErrInternal, "streaming unsupported")
		return
	}
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+mjpegBoundary)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case frame, ok := <-frames:
			if !ok {
				return
			}
			frame = transform(frame)
			if err := rc.SetWriteDeadline(time.Now().Add(frameWriteTimeout)); err != nil {
				return
			}
			_, err := fmt.Fprintf(w, "--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n",
				mjpegBoundary, len(frame))
			if err != nil {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			if _, err := fmt.Fprint(w, "\r\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ServeWS streams a stream's frames as binary WebSocket messages (one JPEG
// per message) until the client disconnects.
func (m *Manager) ServeWS(w http.ResponseWriter, r *http.Request, streamID string) {
	h, ok := m.get(streamID)
	if !ok {
		writeWireError(w, http.StatusNotFound, proto.ErrNotFound, "stream "+streamID+" not found")
		return
	}
	transform, err := viewerTransform(r.URL.Query())
	if err != nil {
		writeWireError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	frames, detach, err := h.attach()
	if err != nil {
		writeAttachError(w, err)
		return
	}
	defer detach()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // origin checks are out of scope for v0 (LAN/tailnet daemon)
	})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusInternalError, "server error")
	ctx := r.Context()
	// Drain reads so pings/close frames are processed.
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-frames:
			if !ok {
				conn.Close(websocket.StatusNormalClosure, "stream closed")
				return
			}
			frame = transform(frame)
			wctx, cancel := context.WithTimeout(ctx, frameWriteTimeout)
			err := conn.Write(wctx, websocket.MessageBinary, frame)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// maxViewerDim caps a viewer's max_dim query param. Simulator captures
// top out well under this, so anything larger buys no fidelity and only
// spends decode/re-encode CPU on the daemon host.
const maxViewerDim = 4096

// viewerTransform reads the optional max_dim / quality query params and
// returns the per-viewer frame transform (identity when neither is set).
// The transform is best-effort: an undecodable frame passes through.
func viewerTransform(q url.Values) (func([]byte) []byte, error) {
	maxDim, err := queryInt(q, "max_dim", 1, maxViewerDim)
	if err != nil {
		return nil, err
	}
	quality, err := queryInt(q, "quality", 1, 100)
	if err != nil {
		return nil, err
	}
	if maxDim == 0 && quality == 0 {
		return func(f []byte) []byte { return f }, nil
	}
	return func(f []byte) []byte {
		out, err := imgutil.Transcode(f, "jpeg", quality, maxDim)
		if err != nil {
			return f
		}
		return out
	}, nil
}

// queryInt parses an optional integer query param, enforcing [min, max].
func queryInt(q url.Values, key string, min, max int) (int, error) {
	s := q.Get(key)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("query param %q must be an integer in %d..%d", key, min, max)
	}
	return n, nil
}

// writeAttachError maps a viewer-attach failure onto the PROTOCOL §1 JSON
// error envelope so reconnect logic can parse it like every other endpoint.
func writeAttachError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrViewerLimit):
		writeWireError(w, http.StatusTooManyRequests, proto.ErrViewerLimit, err.Error())
	case errors.Is(err, ErrStreamClosed):
		writeWireError(w, http.StatusGone, proto.ErrStreamGone, err.Error())
	default:
		writeWireError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
	}
}

func writeWireError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(proto.Error{Code: code, Message: message})
}
