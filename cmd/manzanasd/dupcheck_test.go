package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BariBariGood/manzanas/proto"
)

func TestAlreadyServingDaemon(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"version":%q,"build":"test"}`, proto.Version)
	}))
	defer ts.Close()
	_, port, _ := net.SplitHostPort(ts.Listener.Addr().String())

	if !alreadyServing(":" + port) {
		t.Error("healthz endpoint serving the proto version: want alreadyServing true")
	}
}

func TestAlreadyServingForeign(t *testing.T) {
	cases := map[string]func(t *testing.T) string{
		// A plain TCP listener holding the port but never answering HTTP.
		"not http": func(t *testing.T) string {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					c.Close()
				}
			}()
			_, port, _ := net.SplitHostPort(ln.Addr().String())
			return ":" + port
		},
		// An HTTP service that is not a manzanasd.
		"wrong service": func(t *testing.T) string {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintln(w, `{"status":"healthy"}`)
			}))
			t.Cleanup(ts.Close)
			_, port, _ := net.SplitHostPort(ts.Listener.Addr().String())
			return ":" + port
		},
		// A manzanasd-shape response pinned to a different protocol version
		// (e.g. a future incompatible daemon) still counts as not ours.
		"wrong version": func(t *testing.T) string {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintln(w, `{"ok":true,"version":"v9"}`)
			}))
			t.Cleanup(ts.Close)
			_, port, _ := net.SplitHostPort(ts.Listener.Addr().String())
			return ":" + port
		},
	}
	for name, addr := range cases {
		t.Run(name, func(t *testing.T) {
			if alreadyServing(addr(t)) {
				t.Errorf("want alreadyServing false for %s", name)
			}
		})
	}
}

func TestAlreadyServingMalformedAddr(t *testing.T) {
	if alreadyServing("not-an-addr") {
		t.Error("want alreadyServing false for an unparseable address")
	}
}
