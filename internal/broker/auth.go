package broker

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/BariBariGood/manzanas/proto"
)

// authExempt reports whether a broker request may skip the token check:
// GET /v0/healthz (probes stay credential-free; it leaks only liveness,
// the build version, and the host count), CORS preflights, and the
// dashboard static assets (the dash JS prompts for the token and sends
// it on every API call).
func authExempt(r *http.Request) bool {
	if r.Method == http.MethodOptions {
		return true
	}
	p := r.URL.Path
	if r.Method == http.MethodGet && p == "/v0/healthz" {
		return true
	}
	return p == "/dash" || strings.HasPrefix(p, "/dash/")
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

// authMiddleware wraps h with the shared-token check when a token is
// configured; with no token it returns h unchanged.
func authMiddleware(token string, h http.Handler) http.Handler {
	if token == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := requestToken(r)
		if authExempt(r) ||
			(got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1) {
			h.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, proto.ErrUnauthorized,
			"missing or wrong bearer token (this broker runs with --auth-token; send Authorization: Bearer <token> or ?token=)")
	})
}
