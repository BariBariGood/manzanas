package actions

import (
	"encoding/base64"
	"strings"
	"testing"
)

const fakePNG = "\x89PNG\r\n\x1a\nfake-bytes"

func TestScreenshotViaAXe(t *testing.T) {
	f := newFakeRunner()
	f.writeFile["screenshot"] = fakePNG
	res, err := dispatch(t, testBackend(f), "screenshot", nil)
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	if res.Result["backend"] != "axe" {
		t.Fatalf("backend = %v, want axe", res.Result["backend"])
	}
	b64, _ := res.Result["png_base64"].(string)
	png, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || string(png) != fakePNG {
		t.Fatalf("png payload mismatch: %q (%v)", png, err)
	}
	if res.Result["bytes"].(int) != len(fakePNG) {
		t.Fatalf("bytes = %v", res.Result["bytes"])
	}
	if got := f.argvs()[0]; !strings.HasPrefix(got, "/fake/axe screenshot --output ") {
		t.Fatalf("unexpected argv: %q", got)
	}
}

func TestScreenshotFallsBackToSimctl(t *testing.T) {
	f := newFakeRunner()
	f.writeFile["io"] = fakePNG
	b := NewAXe(WithRunner(f), WithAXePath(""))
	res, err := dispatch(t, b, "screenshot", map[string]any{"inline": false})
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	if res.Result["backend"] != "simctl" {
		t.Fatalf("backend = %v, want simctl", res.Result["backend"])
	}
	// The backend always inlines the PNG; the server journals it and
	// strips png_base64 from the wire response when inline=false.
	if _, ok := res.Result["png_base64"]; !ok {
		t.Fatal("backend should always include png_base64 (server strips for inline=false)")
	}
	if got := f.argvs()[0]; !strings.HasPrefix(got, "xcrun simctl io TEST-UDID screenshot /") {
		t.Fatalf("unexpected argv: %q", got)
	}
}

func TestScreenshotRejectsNonBoolInline(t *testing.T) {
	f := newFakeRunner()
	f.writeFile["screenshot"] = fakePNG
	_, err := dispatch(t, testBackend(f), "screenshot", map[string]any{"inline": "nope"})
	ae, ok := err.(*Error)
	if !ok || ae.Code != "bad_request" {
		t.Fatalf("want bad_request for non-bool inline, got %v", err)
	}
	if len(f.argvs()) != 0 {
		t.Fatalf("invalid request still ran commands: %q", f.argvs())
	}
}
