package server

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/BariBariGood/manzanas/internal/webui"
)

//go:embed web
var dashFS embed.FS

// dashHandler serves the embedded read-only fleet dashboard under /dash/.
// The page talks to the daemon's own v0 API from the same origin; it never
// mutates fleet state. The stylesheet is shared with the broker dashboard
// (internal/webui).
func dashHandler() http.Handler {
	sub, err := fs.Sub(dashFS, "web")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		})
	}
	return webui.Handler("/dash/", sub)
}
