package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// fakeDaemon is an in-process httptest daemon recording every protocol
// request so tests can assert the CLI arg -> wire mapping.
type fakeDaemon struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
	// lease state: getCount lets tests simulate a queued->active transition.
	getCount int
	// actionsNotImplemented makes /v0/actions return 501 like the
	// foundation daemon.
	actionsNotImplemented bool
}

type recordedRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

func newFakeDaemon() *fakeDaemon {
	f := &fakeDaemon{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeOK(w, map[string]any{"ok": true, "version": "v0"})
	})
	mux.HandleFunc("GET /v0/targets", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		writeOK(w, map[string]any{"targets": []proto.Target{{
			UDID: "UDID-1", Kind: proto.TargetSimulator, Name: "iPhone 17 Pro",
			Runtime: "iOS 26.5", DeviceType: "iPhone 17 Pro", State: proto.StateShutdown,
			Labels: []string{"simulator", "ios26", "ios26.5", "iphone-17-pro"},
		}}})
	})
	mux.HandleFunc("POST /v0/leases", func(w http.ResponseWriter, r *http.Request) {
		body := f.record(r, r.Body)
		state, status := proto.LeaseActive, http.StatusCreated
		var qp int
		if p, _ := body["purpose"].(string); p == "queue-me" {
			state, status, qp = proto.LeaseQueued, http.StatusAccepted, 2
		}
		exp := time.Now().Add(5 * time.Minute)
		writeStatus(w, status, proto.Lease{
			ID: "lse_test", TargetUDID: udidFor(state), State: state,
			AgentID: str(body, "agent_id"), TTLSeconds: 300,
			QueuePosition: qp, ExpiresAt: expFor(state, exp),
		})
	})
	mux.HandleFunc("GET /v0/leases", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		writeOK(w, map[string]any{"leases": []proto.Lease{{ID: "lse_test", State: proto.LeaseActive, TargetUDID: "UDID-1"}}})
	})
	mux.HandleFunc("GET /v0/leases/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.mu.Lock()
		f.getCount++
		n := f.getCount
		f.mu.Unlock()
		exp := time.Now().Add(5 * time.Minute)
		if n < 2 {
			writeOK(w, proto.Lease{ID: r.PathValue("id"), State: proto.LeaseQueued, QueuePosition: 1})
			return
		}
		writeOK(w, proto.Lease{ID: r.PathValue("id"), State: proto.LeaseActive,
			TargetUDID: "UDID-1", TTLSeconds: 300, ExpiresAt: &exp})
	})
	mux.HandleFunc("POST /v0/leases/{id}/renew", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, r.Body)
		exp := time.Now().Add(10 * time.Minute)
		writeOK(w, proto.Lease{ID: r.PathValue("id"), State: proto.LeaseActive,
			TargetUDID: "UDID-1", TTLSeconds: 600, ExpiresAt: &exp})
	})
	mux.HandleFunc("DELETE /v0/leases/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		writeOK(w, proto.Lease{ID: r.PathValue("id"), State: proto.LeaseReleased})
	})
	mux.HandleFunc("POST /v0/targets/{udid}/boot", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, r.Body)
		writeStatus(w, http.StatusAccepted, proto.Target{UDID: r.PathValue("udid"), State: proto.StateBooting})
	})
	mux.HandleFunc("POST /v0/targets/{udid}/shutdown", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, r.Body)
		writeStatus(w, http.StatusAccepted, proto.Target{UDID: r.PathValue("udid"), State: proto.StateShuttingDown})
	})
	mux.HandleFunc("POST /v0/actions", func(w http.ResponseWriter, r *http.Request) {
		body := f.record(r, r.Body)
		if f.actionsNotImplemented {
			writeStatus(w, http.StatusNotImplemented, proto.Error{Code: proto.ErrNotImplemented, Message: "actions is not implemented in this build"})
			return
		}
		res := map[string]any{}
		if kind, _ := body["kind"].(string); kind == "screenshot" {
			res["data_base64"] = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
		} else if kind == "observe" {
			res["tree"] = map[string]any{"type": "Application", "label": "Home"}
		}
		writeOK(w, proto.ActionResult{OK: true, Result: res})
	})
	mux.HandleFunc("POST /v0/streams", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, r.Body)
		// Paths are relative to the daemon address, like the real daemon.
		writeOK(w, proto.StreamOffer{StreamID: "stm_1", Format: "mjpeg",
			URL: "/v0/streams/stm_1/ws", MJPEGURL: "/v0/streams/stm_1/mjpeg", ViewURL: "/view/UDID-1"})
	})
	mux.HandleFunc("POST /v0/state/snapshots", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, r.Body)
		writeStatus(w, http.StatusCreated, proto.SnapshotInfo{ID: "snap_1", SourceUDID: "UDID-1"})
	})
	mux.HandleFunc("POST /v0/state/restore", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, r.Body)
		writeOK(w, proto.RestoreResult{OK: true, Snapshot: "snap_1"})
	})
	mux.HandleFunc("POST /v0/state/fixtures", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, r.Body)
		writeOK(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /v0/journal/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		// Serve 3 entries with a server-side clamped page size of 2 and an
		// authoritative next_seq cursor (0 = end), like the real daemon.
		const total, pageSize = 3, 2
		fromSeq, _ := strconv.ParseInt(r.URL.Query().Get("from_seq"), 10, 64)
		if fromSeq < 1 {
			fromSeq = 1
		}
		payloads := []map[string]any{
			{"kind": "tap", "action": "targets.boot", "status": "ok",
				"params": map[string]any{"udid": "UDID-1"}},
			{"kind": "tap", "action": "tap", "status": "error", "error": "element not found"},
			{"kind": "tap", "action": "screenshot", "status": "ok",
				"artifacts": []map[string]any{{"path": "artifacts/deadbeef.png",
					"sha256": "deadbeef00112233445566778899aabb", "bytes": 128}}},
		}
		entries := []map[string]any{}
		seq := fromSeq
		for ; seq <= total && len(entries) < pageSize; seq++ {
			entries = append(entries, map[string]any{
				"ref":  map[string]any{"run_id": r.PathValue("run_id"), "seq": seq},
				"kind": "action", "payload": payloads[seq-1],
			})
		}
		var nextSeq int64
		if seq <= total {
			nextSeq = seq
		}
		writeOK(w, map[string]any{
			"run_id": r.PathValue("run_id"),
			"meta": map[string]any{"format_version": "journal/v0",
				"run_id": r.PathValue("run_id"), "agent_id": "agent-fake",
				"target_name": "iPhone 17 Pro", "target_udid": "UDID-1"},
			"entries": entries, "next_seq": nextSeq})
	})
	mux.HandleFunc("POST /v0/journal/{run_id}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{Method: r.Method,
			Path: r.URL.Path + "?" + r.URL.RawQuery, Body: map[string]any{"bytes": string(data)}})
		f.mu.Unlock()
		writeStatus(w, http.StatusCreated, map[string]any{"artifact": map[string]any{
			"path": "artifacts/deadbeef.png", "sha256": "deadbeef", "bytes": len(data)}})
	})
	f.Server = httptest.NewServer(mux)
	return f
}

func (f *fakeDaemon) record(r *http.Request, body interface{ Read([]byte) (int, error) }) map[string]any {
	var m map[string]any
	if body != nil {
		_ = json.NewDecoder(r.Body).Decode(&m)
	}
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Body: m})
	f.mu.Unlock()
	return m
}

func (f *fakeDaemon) last() recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func writeOK(w http.ResponseWriter, v any) { writeStatus(w, http.StatusOK, v) }
func writeStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }

func udidFor(s proto.LeaseState) string {
	if s == proto.LeaseActive {
		return "UDID-1"
	}
	return ""
}

func expFor(s proto.LeaseState, t time.Time) *time.Time {
	if s == proto.LeaseActive {
		return &t
	}
	return nil
}
