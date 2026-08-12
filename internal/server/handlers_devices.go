package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/BariBariGood/manzanas/proto"
)

// handleDevicesGet serves GET /v0/devices: the current runtime device
// configuration (enumeration flag + per-UDID WDA wiring).
func (s *Server) handleDevicesGet(w http.ResponseWriter, r *http.Request) {
	if s.devmgr == nil {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented,
			"runtime device configuration is not available on this daemon")
		return
	}
	writeJSON(w, http.StatusOK, s.devmgr.Current())
}

// handleDevicesApply serves POST /v0/devices: apply a full DevicesConfig
// to the running daemon (attach/detach devices, rewire WDA, toggle
// enumeration) without a restart. The body replaces the whole config —
// callers wanting a merge GET first. Runtime-only: the daemon's devices
// config file is not rewritten (a SIGHUP re-reads the file and would
// undo the POST; see docs/devices.md).
func (s *Server) handleDevicesApply(w http.ResponseWriter, r *http.Request) {
	if s.devmgr == nil {
		writeError(w, http.StatusNotImplemented, proto.ErrNotImplemented,
			"runtime device configuration is not available on this daemon")
		return
	}
	// The config's launch/forward specs make the daemon spawn host
	// processes (xcodebuild, iproxy), so unlike lease-scoped actions this
	// admin route is never served unauthenticated: it requires the shared
	// bearer token to exist, not merely to match when configured.
	if s.authToken == "" {
		writeError(w, http.StatusForbidden, proto.ErrUnauthorized,
			"POST /v0/devices requires the daemon to be started with --auth-token (it spawns host processes from the submitted config)")
		return
	}
	// Unlike other POSTs an empty body is NOT a benign default here:
	// the zero config means "detach everything", so require an explicit
	// body rather than tearing down every device on a forgotten -d.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest,
			fmt.Sprintf("could not read request body (max %d bytes): %v", maxJSONBodyBytes, err))
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest,
			"request body required (POST replaces the whole devices config; send {\"enabled\":false} to detach everything)")
		return
	}
	// Strict decode, matching devices.Load: with plain Unmarshal a
	// mistyped key ("wdaa") decodes to "no devices" and silently
	// detaches every phone.
	var cfg proto.DevicesConfig
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if err := s.devmgr.Apply(cfg); err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
		return
	}
	s.log.Info("runtime devices config applied", "enabled", cfg.Enabled, "devices", len(cfg.WDA))
	writeJSON(w, http.StatusOK, s.devmgr.Current())
}
