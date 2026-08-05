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
