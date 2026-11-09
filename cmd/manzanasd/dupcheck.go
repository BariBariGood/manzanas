package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// alreadyServing reports whether a manzanasd is already answering the v0
// healthz endpoint on addr. On a failed bind it distinguishes "another
// daemon owns the port" (duplicate service: exit cleanly, launchd leaves
// it down) from "an unrelated process holds the port" (a real config
// error that must stay fatal). Healthz is auth-exempt, so the probe works
// whether or not the running daemon has a bearer token.
func alreadyServing(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/v0/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var body struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil {
		return false
	}
	return resp.StatusCode == http.StatusOK && body.OK && body.Version == proto.Version
}
