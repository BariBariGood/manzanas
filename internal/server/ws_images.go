package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/BariBariGood/manzanas/internal/state"
	"github.com/BariBariGood/manzanas/proto"
)

// wsImageError maps image-store errors to WS error envelopes.
func wsImageError(id string, err error) proto.Envelope {
	switch {
	case errors.Is(err, state.ErrImageNotFound):
		return wsError(id, proto.ErrNotFound, err.Error())
	case errors.Is(err, state.ErrBadImageRequest):
		return wsError(id, proto.ErrBadRequest, err.Error())
	case errors.Is(err, state.ErrSlimUnavailable):
		return wsError(id, proto.ErrUnavailable, err.Error())
	case errors.Is(err, state.ErrTargetBusy):
		return wsError(id, proto.ErrTargetBusy, err.Error())
	case errors.Is(err, state.ErrImageCorrupt):
		return wsError(id, proto.ErrUnavailable, err.Error())
	default:
		return wsError(id, proto.ErrInternal, err.Error())
	}
}

// dispatchImagesWS handles the images.* WS methods (mirroring the REST
// handlers in handlers_images.go).
func (s *Server) dispatchImagesWS(ctx context.Context, env proto.Envelope) proto.Envelope {
	if s.images == nil {
		return wsError(env.ID, proto.ErrNotImplemented, "images are not implemented in this build")
	}
	switch env.Method {
	case proto.MethodImagesBuild:
		var req proto.ImageBuildRequest
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		info, err := s.images.Build(ctx, req)
		if err != nil {
			return wsImageError(env.ID, err)
		}
		return wsResult(env.ID, info)

	case proto.MethodImagesList:
		imgs, err := s.images.List(ctx)
		if err != nil {
			return wsImageError(env.ID, err)
		}
		if imgs == nil {
			imgs = []proto.ImageInfo{}
		}
		return wsResult(env.ID, map[string]any{"images": imgs})

	case proto.MethodImagesStamp:
		var req struct {
			ID string `json:"id"`
			proto.ImageStampRequest
		}
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		start := time.Now()
		info, created, err := s.images.Stamp(ctx, req.ID, req.Count, req.NamePrefix)
		if err != nil {
			return wsImageError(env.ID, err)
		}
		return wsResult(env.ID, proto.ImageStampResult{
			OK:         true,
			ImageID:    info.ID,
			Created:    created,
			DurationMS: time.Since(start).Milliseconds(),
		})

	case proto.MethodImagesDelete:
		var req struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(env.Params, &req); err != nil {
			return wsError(env.ID, proto.ErrBadRequest, err.Error())
		}
		if err := s.images.Delete(ctx, req.ID); err != nil {
			return wsImageError(env.ID, err)
		}
		return wsResult(env.ID, map[string]any{"ok": true})

	default:
		return wsError(env.ID, proto.ErrBadRequest, "unknown method: "+env.Method)
	}
}
