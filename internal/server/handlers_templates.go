package server

import (
	"net/http"

	"github.com/BariBariGood/manzanas/internal/templates"
	"github.com/BariBariGood/manzanas/proto"
)

// Template library endpoints (PROTOCOL.md §5.4): list and delete the
// cross-run templates saved by the save_template action. Leaseless, like
// the journal reads: the library is host-level state, not a per-lease
// resource.

// handleTemplatesList maps GET /v0/templates.
func (s *Server) handleTemplatesList(w http.ResponseWriter, r *http.Request) {
	if s.templates == nil {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented,
			"the template library is not configured on this daemon (--templates-dir)")
		return
	}
	list, err := s.templates.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	if list == nil {
		list = []templates.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": list})
}

// handleTemplatesDelete maps DELETE /v0/templates/{bundle_id}/{name}
// (?device_class=<backend>/<img_w> scopes to one class; omitted removes
// the name across all classes).
func (s *Server) handleTemplatesDelete(w http.ResponseWriter, r *http.Request) {
	if s.templates == nil {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented,
			"the template library is not configured on this daemon (--templates-dir)")
		return
	}
	bundleID := r.PathValue("bundle_id")
	name := r.PathValue("name")
	class := r.URL.Query().Get("device_class")
	// Validate the caller-supplied components here so bad input is 400,
	// while any error Remove itself reports is a host-side failure (500)
	// — possibly after some templates were already deleted, so surface
	// the partial count.
	if err := templates.ValidateBundleID(bundleID); err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	if err := templates.ValidateName(name); err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	if class != "" {
		if err := templates.ValidateClass(class); err != nil {
			writeError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
			return
		}
	}
	removed, err := s.templates.Remove(bundleID, name, class)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": proto.ErrInternal, "message": err.Error(), "removed": removed,
		})
		return
	}
	if removed == 0 {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "no matching template")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bundle_id": bundleID, "name": name, "removed": removed,
	})
}
