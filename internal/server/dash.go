package server

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web
var dashFS embed.FS

// dashHandler serves the embedded read-only fleet dashboard under /dash/.
// The page talks to the daemon's own v0 API from the same origin; it never
// mutates fleet state.
func dashHandler() http.Handler {
	sub, err := fs.Sub(dashFS, "web")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		})
	}
	return http.StripPrefix("/dash/", http.FileServer(http.FS(sub)))
}
