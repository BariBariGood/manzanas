package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/internal/stream"
	"github.com/BariBariGood/manzanas/proto"
)

// resolveStreamTarget maps a StreamRequest to a target UDID. Viewing is
// read-only so no lease is required: the request may name the target by
// UDID directly, or by an active lease ID.
func (s *Server) resolveStreamTarget(req proto.StreamRequest) (string, string, error) {
	switch {
	case req.UDID != "":
		return req.UDID, "", nil
	case req.LeaseID != "":
		l, err := s.leases.Get(req.LeaseID)
		if err != nil {
			return "", proto.ErrNotFound, errors.New("lease not found")
		}
		if l.State != proto.LeaseActive {
			return "", proto.ErrLeaseExpired, errors.New("lease is not active")
		}
		return l.TargetUDID, "", nil
	default:
		return "", proto.ErrBadRequest, errors.New("udid or lease_id is required")
	}
}

// openStream validates the target and negotiates a stream, annotating the
// offer with the target's current lease holder (if any).
func (s *Server) openStream(ctx context.Context, req proto.StreamRequest) (proto.StreamOffer, string, error) {
	udid, code, err := s.resolveStreamTarget(req)
	if err != nil {
		return proto.StreamOffer{}, code, err
	}
	t, err := s.reg.Get(ctx, udid)
	if err != nil {
		var nf *registry.NotFoundError
		if errors.As(err, &nf) {
			return proto.StreamOffer{}, proto.ErrNotFound, err
		}
		return proto.StreamOffer{}, proto.ErrInternal, err
	}
	if t.Kind == proto.TargetDevice {
		// The stream manager captures via simctl; a connected device
		// reports Booted but cannot be streamed.
		return proto.StreamOffer{}, proto.ErrNotImplemented,
			fmt.Errorf("target %s is a physical device; streaming is simulator-only", udid)
	}
	if t.State != proto.StateBooted {
		return proto.StreamOffer{}, proto.ErrTargetBusy,
			fmt.Errorf("target %s is %s; streaming requires a booted target", udid, t.State)
	}
	if s.parked != nil && s.parked(udid) {
		// A parked (SIGSTOPped) warm-pool sim reports Booted but every
		// frame capture against it hangs; a lease thaws it.
		return proto.StreamOffer{}, proto.ErrTargetBusy,
			fmt.Errorf("target %s is parked in the warm pool; acquire a lease to thaw it before streaming", udid)
	}
	offer, err := s.streamer.Open(ctx, udid, req)
	switch {
	case errors.Is(err, stream.ErrUnsupportedFormat), errors.Is(err, stream.ErrBadStreamParam):
		return proto.StreamOffer{}, proto.ErrBadRequest, err
	case errors.Is(err, stream.ErrStreamLimit):
		return proto.StreamOffer{}, proto.ErrStreamLimit, err
	case err != nil:
		return proto.StreamOffer{}, proto.ErrInternal, err
	}
	if holder, ok := s.leases.Active(udid); ok {
		if req.LeaseID != holder.ID {
			// The lease ID is a capability token; only echo it back to
			// a caller that already presented it (UDID-addressed callers
			// like the dashboard get the holder's metadata without it).
			holder.ID = ""
		}
		offer.Holder = &holder
	}
	return offer, "", nil
}

func streamErrStatus(code string) int {
	switch code {
	case proto.ErrNotFound:
		return http.StatusNotFound
	case proto.ErrLeaseExpired:
		return http.StatusGone
	case proto.ErrBadRequest:
		return http.StatusBadRequest
	case proto.ErrStreamLimit:
		return http.StatusTooManyRequests
	case proto.ErrTargetBusy:
		return http.StatusConflict
	case proto.ErrNotImplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

// corsStreams marks stream negotiation as cross-origin-readable: the
// broker's aggregated dashboard is served from the broker's origin but
// negotiates streams directly against each owning daemon (media never
// flows through the broker). Only this endpoint gets CORS headers — the
// MJPEG media itself rides in an <img>, which needs none. With no
// --auth-token the wildcard preserves the open tailnet-only model; with
// a token the request only reaches this handler after the auth
// middleware validated it, and the origin is echoed back (never *) so
// only token-holding pages can read the response.
func (s *Server) corsStreams(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.setStreamsCORS(w, r)
		h(w, r)
	}
}

func (s *Server) setStreamsCORS(w http.ResponseWriter, r *http.Request) {
	if s.authToken == "" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
}

// handleStreamsPreflight answers the browser's CORS preflight for
// POST /v0/streams (a JSON content type makes it a non-simple request).
// Preflights are auth-exempt by construction (they carry no credentials);
// the Authorization header is allowed so token-bearing pages can follow up.
func (s *Server) handleStreamsPreflight(w http.ResponseWriter, r *http.Request) {
	s.setStreamsCORS(w, r)
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleOpenStream(w http.ResponseWriter, r *http.Request) {
	var req proto.StreamRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	offer, code, err := s.openStream(r.Context(), req)
	if err != nil {
		writeError(w, streamErrStatus(code), code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, offer)
}

func (s *Server) handleCloseStream(w http.ResponseWriter, r *http.Request) {
	if err := s.streamer.Close(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStreamMJPEG(w http.ResponseWriter, r *http.Request) {
	s.streamer.ServeMJPEG(w, r, r.PathValue("id"))
}

func (s *Server) handleStreamWS(w http.ResponseWriter, r *http.Request) {
	s.streamer.ServeWS(w, r, r.PathValue("id"))
}

func (s *Server) handleViewPage(w http.ResponseWriter, r *http.Request) {
	stream.ServeViewPage(w, r)
}
