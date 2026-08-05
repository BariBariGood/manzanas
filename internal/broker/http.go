package broker

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/BariBariGood/manzanas/proto"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, proto.Error{Code: code, Message: msg})
}

// jsonErrorEnvelope rewrites the mux's built-in plain-text 404/405
// responses into the protocol's JSON error envelope, so unrouted paths
// and wrong methods answer with the same shape as every other error.
func jsonErrorEnvelope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&envelopeWriter{ResponseWriter: w}, r)
	})
}

// envelopeWriter intercepts 404/405 responses that carry the mux's
// text/plain content type; handler-written JSON errors pass through.
type envelopeWriter struct {
	http.ResponseWriter
	intercepted bool
	wroteHeader bool
}

func (w *envelopeWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		if ct := w.Header().Get("Content-Type"); ct == "" || strings.HasPrefix(ct, "text/plain") {
			w.intercepted = true
			code, msg := proto.ErrNotFound, "unknown route"
			if status == http.StatusMethodNotAllowed {
				code, msg = proto.ErrBadRequest, "method not allowed"
			}
			writeError(w.ResponseWriter, status, code, msg)
			return
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *envelopeWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.intercepted {
		return len(b), nil // drop the mux's plain-text body
	}
	return w.ResponseWriter.Write(b)
}

// decodeJSON decodes the request body into v; an empty body leaves v at
// its zero value.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	err := json.NewDecoder(r.Body).Decode(v)
	if err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, proto.ErrBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}
