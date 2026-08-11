package server

import (
	"net/http"
	"strings"
	"testing"
)

// The broker dashboard negotiates streams cross-origin (its page origin
// is the broker; POST /v0/streams goes to the owning daemon), so stream
// negotiation must carry CORS headers — including on error responses,
// so the dash can read the failure.
func TestStreamsCORS(t *testing.T) {
	ts := newTestServer(t) // no streamer wired: POST answers 501

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v0/streams",
		strings.NewReader(`{"udid":"nope","format":"mjpeg"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://broker.example:7440")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("POST /v0/streams: status %d, want 501", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("POST /v0/streams: Access-Control-Allow-Origin %q, want *", got)
	}

	// Preflight: a JSON POST is a non-simple request, so the browser
	// sends OPTIONS first.
	req, err = http.NewRequest(http.MethodOptions, ts.URL+"/v0/streams", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://broker.example:7440")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Content-Type")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS /v0/streams: status %d, want 204", resp.StatusCode)
	}
	for h, want := range map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Methods": "POST, OPTIONS",
		"Access-Control-Allow-Headers": "Content-Type, Authorization",
	} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("OPTIONS /v0/streams: %s %q, want %q", h, got, want)
		}
	}
}
