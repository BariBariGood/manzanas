package server

import (
	"net/http"

	"github.com/BariBariGood/manzanas/internal/elementmap"
	"github.com/BariBariGood/manzanas/proto"
)

// Element map endpoints (PROTOCOL.md §5.4): list and delete the
// cross-run elements learned from successful tap_element /
// type_into_element resolutions. Leaseless, like the template library:
// the map is host-level state, not a per-lease resource. Element names
// are free-form matcher strings (`label == "Sign In"`), so deletes take
// the name as a query parameter instead of a path segment.

// handleElementMapList maps GET /v0/elementmap.
func (s *Server) handleElementMapList(w http.ResponseWriter, r *http.Request) {
	if s.elementMap == nil {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented,
			"the element map is not configured on this daemon (--element-map / --element-map-dir)")
		return
	}
	list, err := s.elementMap.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, proto.ErrInternal, err.Error())
		return
	}
	if list == nil {
		list = []elementmap.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"elements": list})
}

// handleElementMapDelete maps DELETE /v0/elementmap/{bundle_id}?name=...
// (?device_class=<backend>/<img_w> scopes to one class; omitted removes
// the name across all classes).
func (s *Server) handleElementMapDelete(w http.ResponseWriter, r *http.Request) {
	if s.elementMap == nil {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented,
			"the element map is not configured on this daemon (--element-map / --element-map-dir)")
		return
	}
	bundleID := r.PathValue("bundle_id")
	name := r.URL.Query().Get("name")
	class := r.URL.Query().Get("device_class")
	if err := elementmap.ValidateBundleID(bundleID); err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest,
			"query parameter \"name\" is required")
		return
	}
	if err := elementmap.ValidateName(name); err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	if class != "" {
		if err := elementmap.ValidateClass(class); err != nil {
			writeError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
			return
		}
	}
	removed, err := s.elementMap.Remove(bundleID, name, class)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": proto.ErrInternal, "message": err.Error(), "removed": removed,
		})
		return
	}
	if removed == 0 {
		writeError(w, http.StatusNotFound, proto.ErrNotFound, "no matching element")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bundle_id": bundleID, "name": name, "removed": removed,
	})
}
