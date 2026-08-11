package broker

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/BariBariGood/manzanas/internal/webui"
)

//go:embed web
var dashFS embed.FS

// dashHandler serves the embedded read-only fleet dashboard under /dash/.
// The page aggregates the whole fleet through the broker's own endpoints
// (/v0/fleet/hosts, /v0/targets, /v0/leases) and negotiates live streams
// directly against each owning daemon's host_addr — media never flows
// through the broker. The stylesheet is shared with the daemon dashboard
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
