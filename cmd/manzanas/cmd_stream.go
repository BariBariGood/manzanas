package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/BariBariGood/manzanas/proto"
)

// cmdStream implements `manzanas stream url` (POST /v0/streams): negotiates
// a stream and prints its URL for a browser or viewer.
func cmdStream(ctx context.Context, app *appEnv, args []string) error {
	sub, err := requireArg(args, 0, "stream subcommand (url)")
	if err != nil {
		return err
	}
	if sub != "url" {
		return fmt.Errorf("unknown stream subcommand %q", sub)
	}
	fs := app.newFlagSet("stream url")
	lease := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID")
	udid := fs.String("udid", "", "target UDID (viewing is read-only; no lease needed)")
	format := fs.String("format", "mjpeg", "stream format: mjpeg or h264")
	maxFPS := fs.Int("max-fps", 0, "cap the frame rate")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("stream url: unexpected argument %q", fs.Arg(0))
	}
	req := proto.StreamRequest{Format: *format, MaxFPS: *maxFPS}
	switch {
	case *udid != "":
		req.UDID = *udid
	case *lease != "":
		req.LeaseID = *lease
	default:
		return fmt.Errorf("stream url: --udid or --lease (or $MANZANAS_LEASE) is required")
	}
	offer, err := app.client.OpenStream(ctx, req)
	if err != nil {
		return err
	}
	// The daemon returns paths relative to its own address; absolutize them
	// so the printed value is directly usable in a browser or agent. For a
	// broker-acquired lease the client routed the request to the owning
	// daemon, so absolutize against that address, not the broker's.
	base := app.client.AddrForLease(req.LeaseID)
	offer.ViewURL = withToken(absURL(base, offer.ViewURL), app.token)
	offer.MJPEGURL = withToken(absURL(base, offer.MJPEGURL), app.token)
	offer.URL = withToken(wsURL(absURL(base, offer.URL)), app.token)
	return app.emit(offer, func(w io.Writer) {
		link := offer.ViewURL
		if link == "" {
			link = offer.MJPEGURL
		}
		if link == "" {
			link = offer.URL
		}
		fmt.Fprintln(w, link)
	})
}

// withToken appends a ?token= query parameter so the printed URL works
// against an --auth-token daemon from contexts that cannot set an
// Authorization header (a browser <img>, a bare WebSocket URL).
func withToken(u, token string) string {
	if u == "" || token == "" {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "token=" + url.QueryEscape(token)
}

func absURL(base, p string) string {
	if p == "" || strings.Contains(p, "://") {
		return p
	}
	return base + p
}

// wsURL rewrites an http(s) scheme to ws(s) for the WebSocket endpoint.
func wsURL(u string) string {
	if s, ok := strings.CutPrefix(u, "https://"); ok {
		return "wss://" + s
	}
	if s, ok := strings.CutPrefix(u, "http://"); ok {
		return "ws://" + s
	}
	return u
}
