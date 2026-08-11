package server

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/BariBariGood/manzanas/proto"
)

// SetAuthToken enables shared bearer-token auth: when token is non-empty,
// every endpoint except GET /v0/healthz, the CORS preflight, and the
// dashboard/view static assets requires the token — via
// `Authorization: Bearer <token>` or a `?token=` query parameter (for
// browser contexts that cannot set headers: the MJPEG <img>, WebSocket
// URLs). Empty (the default) keeps the open tailnet-only trust model.
func (s *Server) SetAuthToken(token string) { s.authToken = token }

// authExempt reports whether the request may skip the token check:
//   - GET /v0/healthz — health probes (brokers, load balancers) stay
//     credential-free; it leaks only liveness and the build version.
//   - OPTIONS — CORS preflights never carry credentials by spec.
//   - the dashboard and view-page static assets — the page itself is
//     public shell; every API call it makes is authenticated, and its
//     JS prompts for the token.
func authExempt(r *http.Request) bool {
	if r.Method == http.MethodOptions {
		return true
	}
	p := r.URL.Path
	if r.Method == http.MethodGet && p == "/v0/healthz" {
		return true
	}
	return p == "/dash" || strings.HasPrefix(p, "/dash/") ||
		strings.HasPrefix(p, "/view/")
}

// requestToken extracts the presented token: the Authorization bearer
// header, falling back to the token query parameter.
func requestToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if t, ok := strings.CutPrefix(h, "Bearer "); ok {
			return t
		}
		return ""
	}
	return r.URL.Query().Get("token")
}

// tokenMatches constant-time-compares the presented token.
func tokenMatches(token string, r *http.Request) bool {
	got := requestToken(r)
	return got != "" &&
		subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// authMiddleware wraps h with the shared-token check when a token is
// configured; with no token it returns h unchanged.
func (s *Server) authMiddleware(h http.Handler) http.Handler {
	if s.authToken == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authExempt(r) || tokenMatches(s.authToken, r) {
			h.ServeHTTP(w, r)
			return
		}
		// Stream negotiation is called cross-origin by the broker dash;
		// CORS headers on the 401 let the page read it and prompt for
		// the token instead of surfacing an opaque network error.
		if r.URL.Path == "/v0/streams" {
			s.setStreamsCORS(w, r)
		}
		writeError(w, http.StatusUnauthorized, proto.ErrUnauthorized,
			"missing or wrong bearer token (this daemon runs with --auth-token; send Authorization: Bearer <token> or ?token=)")
	})
}
