package stream

import (
	"embed"
	"net/http"
)

//go:embed web/view.html
var webFS embed.FS

// ServeViewPage serves the static browser view page for a target. The page
// negotiates its own stream via POST /v0/streams and renders the MJPEG URL.
func ServeViewPage(w http.ResponseWriter, r *http.Request) {
	page, err := webFS.ReadFile("web/view.html")
	if err != nil {
		http.Error(w, "view page unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}
