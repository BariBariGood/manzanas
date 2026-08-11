// Package webui carries the static assets shared by the manzanasd and
// manzanas-broker embedded dashboards (the stylesheet), and serves a
// dashboard's own file tree with those shared assets as a fallback.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var sharedFS embed.FS

// Handler serves the dashboard app under prefix (e.g. "/dash/"). Files
// the app carries itself win; anything else falls back to the shared
// assets (style.css).
func Handler(prefix string, app fs.FS) http.Handler {
	shared, err := fs.Sub(sharedFS, "assets")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		})
	}
	return http.StripPrefix(prefix,
		http.FileServer(http.FS(overlayFS{app: app, fallback: shared})))
}

// overlayFS resolves names against app first, then the shared assets.
type overlayFS struct{ app, fallback fs.FS }

func (o overlayFS) Open(name string) (fs.File, error) {
	if f, err := o.app.Open(name); err == nil {
		return f, nil
	}
	return o.fallback.Open(name)
}
