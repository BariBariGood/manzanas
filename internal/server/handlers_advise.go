package server

import (
	"net/http"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// handleAdvisePool serves POST /v0/pool/advise: record a scheduler's
// advisory view of warm-pool demand. Advice is informational — the daemon
// keeps final say over its pool via its capacity class and safety gates,
// and never grows or shrinks anything on a scheduler's word alone. The
// latest accepted advice is surfaced on GET /v0/status (pool_advice) so
// operators and dashboards can see what the fleet scheduler is asking for.
func (s *Server) handleAdvisePool(w http.ResponseWriter, r *http.Request) {
	var req proto.PoolAdviseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	for _, c := range req.Classes {
		if c.Action != proto.AdviceGrow && c.Action != proto.AdviceShrink {
			writeError(w, http.StatusBadRequest, proto.ErrBadRequest,
				"unknown advice action "+string(c.Action))
			return
		}
		if c.Action == proto.AdviceGrow && len(c.Labels) == 0 {
			writeError(w, http.StatusBadRequest, proto.ErrBadRequest,
				"grow advice requires a label class")
			return
		}
	}
	st := &proto.PoolAdviceState{
		Source:        req.Source,
		ReceivedAt:    time.Now().UTC(),
		WindowSeconds: req.WindowSeconds,
		Classes:       req.Classes,
	}
	s.adviceMu.Lock()
	s.advice = st
	s.adviceMu.Unlock()
	s.log.Info("pool advice received", "source", req.Source, "classes", len(req.Classes))
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "acted": false})
}

// adviceSnapshot returns the most recent accepted advice, or nil.
func (s *Server) adviceSnapshot() *proto.PoolAdviceState {
	s.adviceMu.Lock()
	defer s.adviceMu.Unlock()
	return s.advice
}
