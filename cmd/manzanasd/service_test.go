package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceArgsAbsolutizesPathFlags(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got := serviceArgs([]string{
		"--devices-config", "devices.json",
		"--state-dir=./state",
		"--addr", ":7433",
		"--journal-dir", "/abs/journal",
		"--lock-dir", "",
	})
	want := []string{
		"--devices-config", filepath.Join(cwd, "devices.json"),
		"--state-dir=" + filepath.Join(cwd, "state"),
		"--addr", ":7433",
		"--journal-dir", "/abs/journal",
		"--lock-dir", "",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("serviceArgs = %v, want %v", got, want)
	}
}

func TestServiceArgsStripsInstallService(t *testing.T) {
	got := serviceArgs([]string{"--addr", ":7433", "--install-service", "--devices", "-install-service=true", "--device-wda", "UD1=http://x"})
	want := "--addr :7433 --devices --device-wda UD1=http://x"
	if strings.Join(got, " ") != want {
		t.Errorf("serviceArgs = %v, want %s", got, want)
	}
}

func TestLaunchdPlist(t *testing.T) {
	p := launchdPlist("com.baribarigood.manzanasd", "/Users/x/bin/manzanasd",
		[]string{"--addr", ":7433", "--device-wda", "UD1=http://127.0.0.1:8100?a=1&b=2"},
		"/Users/x/.manzanasd/logs", "/Users/x/.manzanasd", "/opt/homebrew/bin:/usr/bin",
		map[string]string{"MANZANASD_AUTH_TOKEN": "tok&1"})
	for _, frag := range []string{
		"<string>com.baribarigood.manzanasd</string>",
		"<string>/Users/x/bin/manzanasd</string>",
		"<string>--addr</string>",
		"<string>:7433</string>",
		"<string>UD1=http://127.0.0.1:8100?a=1&amp;b=2</string>",
		"<key>KeepAlive</key>",
		"<key>SuccessfulExit</key>",
		"<key>RunAtLoad</key>",
		"<key>WorkingDirectory</key>",
		"<string>/Users/x/.manzanasd</string>",
		"<key>PATH</key>",
		"<string>/opt/homebrew/bin:/usr/bin</string>",
		"<key>MANZANASD_AUTH_TOKEN</key>",
		"<string>tok&amp;1</string>",
		"<key>ProcessType</key>",
		"manzanasd.out.log</string>",
		"manzanasd.err.log</string>",
	} {
		if !strings.Contains(p, frag) {
			t.Errorf("plist missing %q:\n%s", frag, p)
		}
	}
	if strings.Contains(p, "install-service") {
		t.Error("plist contains install-service")
	}
}
