package wda

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestParseForward(t *testing.T) {
	f, err := ParseForward("UD1", "8100:8200")
	if err != nil {
		t.Fatal(err)
	}
	if f.Local != 8100 || f.Remote != 8200 || f.UDID != "UD1" {
		t.Errorf("parsed %+v", f)
	}
	for _, bad := range []string{"", "8100", "x:8100", "8100:y", "0:8100", "8100:70000"} {
		if _, err := ParseForward("UD1", bad); err == nil {
			t.Errorf("ParseForward(%q) succeeded, want error", bad)
		}
	}
}

func TestForwardCommandPrefersIproxy(t *testing.T) {
	f := NewForwardLauncher("UD1", 8100, 8100)
	f.lookPath = func(name string) (string, error) {
		if name == "iproxy" {
			return "/opt/bin/iproxy", nil
		}
		return "", errors.New("not found")
	}
	name, args, err := f.command()
	if err != nil {
		t.Fatal(err)
	}
	if name != "/opt/bin/iproxy" || strings.Join(args, " ") != "8100 8100 -u UD1" {
		t.Errorf("command = %s %v", name, args)
	}
}

func TestForwardCommandFallsBackToPymobiledevice3(t *testing.T) {
	f := NewForwardLauncher("UD1", 8100, 8200)
	f.lookPath = func(name string) (string, error) {
		if name == "pymobiledevice3" {
			return "/opt/bin/pymobiledevice3", nil
		}
		return "", errors.New("not found")
	}
	name, args, err := f.command()
	if err != nil {
		t.Fatal(err)
	}
	if name != "/opt/bin/pymobiledevice3" || strings.Join(args, " ") != "usbmux forward 8100 8200 --serial UD1" {
		t.Errorf("command = %s %v", name, args)
	}
}

func TestForwardCommandNoForwarder(t *testing.T) {
	f := NewForwardLauncher("UD1", 8100, 8100)
	f.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if _, _, err := f.command(); err == nil || !strings.Contains(err.Error(), "no usbmux forwarder") {
		t.Errorf("command err = %v", err)
	}
}

func TestForwardLaunchStopsPredecessor(t *testing.T) {
	f := NewForwardLauncher("UD1", 8100, 8100)
	f.lookPath = func(string) (string, error) { return "/opt/bin/iproxy", nil }
	var stops int
	f.start = func(name string, args ...string) (func(), error) {
		return func() { stops++ }, nil
	}
	if err := f.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stops != 1 {
		t.Errorf("relaunch stopped %d children, want 1", stops)
	}
	f.Stop()
	if stops != 2 {
		t.Errorf("Stop left %d children stopped, want 2", stops)
	}
	f.Stop() // idempotent
	if stops != 2 {
		t.Errorf("second Stop re-stopped: %d", stops)
	}
}

func TestForwardHealthy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	f := NewForwardLauncher("UD1", port, 8100)
	if !f.Healthy(context.Background()) {
		t.Error("Healthy = false with a live listener")
	}
	ln.Close()
	if f.Healthy(context.Background()) {
		t.Error("Healthy = true with no listener")
	}
}
