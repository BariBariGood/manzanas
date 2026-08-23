package server

import (
	"context"
	"net/http"

	"github.com/BariBariGood/manzanas/proto"
)

// DoctorFunc runs the host's setup diagnosis for GET /v0/doctor. target,
// when non-empty, restricts per-device checks to that UDID.
type DoctorFunc func(ctx context.Context, target string) proto.DoctorReport

// SetDoctor wires the doctor aggregator; when unset GET /v0/doctor
// answers not_implemented (older daemons answer 404 — clients must
// tolerate both).
func (s *Server) SetDoctor(fn DoctorFunc) { s.doctor = fn }

// handleDoctor serves GET /v0/doctor: the host setup diagnosis
// (`manzanas doctor`). Always 200 with the report — failed checks are
// data, carried in the body's `ok` and per-check statuses.
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if s.doctor == nil {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented,
			"doctor is not implemented in this build")
		return
	}
	writeJSON(w, http.StatusOK, s.doctor(r.Context(), r.URL.Query().Get("target")))
}
