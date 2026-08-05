package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/BariBariGood/manzanas/internal/state"
	"github.com/BariBariGood/manzanas/proto"
)

// Golden-image routes (owned by the state slice). Images are fleet-level
// resources (like targets), not per-lease state: build/stamp/delete
// operate only on sims the image flow itself creates, so no lease guard
// applies. See docs/images.md.

// writeImageError maps image-store errors to wire errors.
func writeImageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, state.ErrImageNotFound):
		writeError(w, http.StatusNotFound, proto.ErrNotFound, err.Error())
	case errors.Is(err, state.ErrBadImageRequest):
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
	case errors.Is(err, state.ErrSlimUnavailable):
		writeError(w, http.StatusServiceUnavailable, proto.ErrUnavailable, err.Error())
	case errors.Is(err, state.ErrTargetBusy):
		writeError(w, http.StatusConflict, proto.ErrTargetBusy, err.Error())
	case errors.Is(err, state.ErrImageCorrupt):
		// Bad on-disk state (swapped/corrupted archive), not a daemon bug:
		// the image is unusable until rebuilt or deleted.
		writeError(w, http.StatusConflict, proto.ErrUnavailable, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
	}
}

func (s *Server) handleImagesBuild(w http.ResponseWriter, r *http.Request) {
	var req proto.ImageBuildRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	info, err := s.images.Build(r.Context(), req)
	if err != nil {
		writeImageError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) handleImagesList(w http.ResponseWriter, r *http.Request) {
	imgs, err := s.images.List(r.Context())
	if err != nil {
		writeImageError(w, err)
		return
	}
	if imgs == nil {
		imgs = []proto.ImageInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": imgs})
}

func (s *Server) handleImagesStamp(w http.ResponseWriter, r *http.Request) {
	var req proto.ImageStampRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	start := time.Now()
	info, created, err := s.images.Stamp(r.Context(), id, req.Count, req.NamePrefix)
	if err != nil {
		writeImageError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, proto.ImageStampResult{
		OK:         true,
		ImageID:    info.ID,
		Created:    created,
		DurationMS: time.Since(start).Milliseconds(),
	})
}

func (s *Server) handleImagesDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.images.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeImageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
