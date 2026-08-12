package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// serviceLabel is the LaunchAgent label --install-service manages; it
// matches deploy/install.sh so re-running either refreshes the same job.
const serviceLabel = "com.baribarigood.manzanasd"

// installLaunchdService implements `manzanasd --install-service`: write
// (or refresh) the per-user LaunchAgent plist running this binary with
// the current flags, then (re)load it. Idempotent: re-running with new
// flags rewrites the plist and restarts the job; nothing else on the
// host is touched (per-user launchd domain only, no sudo).
func installLaunchdService(args []string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("launchd services are macOS-only (GOOS=%s)", runtime.GOOS)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// Log where deploy/install.sh's daily copy-truncate rotation agent
	// already looks, so an --install-service daemon's logs stay bounded
	// on hosts set up either way.
	logDir := filepath.Join(home, ".manzanasd", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	// Match deploy/install.sh: a stable, installer-created directory —
	// launchd refuses to spawn a job whose WorkingDirectory is gone,
	// which a build/checkout cwd eventually is.
	workDir := filepath.Join(home, ".manzanasd")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		workDir = home
	}
	// A token passed as --auth-token would land verbatim in
	// ProgramArguments, where any local user can read it via ps or
	// launchctl print; carry it in the plist's (0600) env instead.
	svcArgs, secrets := splitSecretFlags(serviceArgs(args))
	env := serviceEnv()
	if tok, ok := secrets["auth-token"]; ok {
		env["MANZANASD_AUTH_TOKEN"] = tok
	}
	plist := launchdPlist(serviceLabel, self, svcArgs, logDir, workDir, servicePATH(home), env)
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, serviceLabel+".plist")
	// Stash the previous plist: a refresh whose new definition launchd
	// rejects must be able to put the old service back rather than leave
	// the daemon booted out and stopped.
	prevPlist, _ := os.ReadFile(path)
	// 0600: the plist embeds the MANZANASD_* env snapshot, which can
	// include MANZANASD_AUTH_TOKEN. launchd loads per-user LaunchAgents
	// as the owning user, so it does not need group/world read.
	if err := os.WriteFile(path, []byte(plist), 0o600); err != nil {
		return err
	}

	domain := "gui/" + strconv.Itoa(os.Getuid())
	// bootout is best-effort: the job may not be loaded yet, and launchctl
	// has no idempotent "load or reload".
	_ = exec.Command("launchctl", "bootout", domain+"/"+serviceLabel).Run()
	// bootout returns before the job is fully gone; bootstrap can
	// transiently fail with "5: Input/output error" (same race
	// deploy/install.sh retries around). A failed refresh must not leave
	// the daemon booted out and stopped.
	var bootErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		out, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput()
		if err == nil {
			bootErr = nil
			break
		}
		bootErr = fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if bootErr != nil {
		// Best-effort rollback to the previous definition so a rejected
		// refresh degrades to "old service still running", not "no
		// service at all".
		if len(prevPlist) > 0 {
			if err := os.WriteFile(path, prevPlist, 0o600); err == nil {
				if exec.Command("launchctl", "bootstrap", domain, path).Run() == nil {
					return fmt.Errorf("%w (previous service definition restored)", bootErr)
				}
			}
		}
		return bootErr
	}
	if out, err := exec.Command("launchctl", "kickstart", "-k", domain+"/"+serviceLabel).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("installed %s\n  binary: %s\n  args:   %s\n  logs:   %s\n", path, self, strings.Join(svcArgs, " "), logDir)
	return nil
}

// splitSecretFlags removes secret-bearing flags from args, returning the
// scrubbed argv and the extracted flag values by name. Secrets must ride
// in the plist's EnvironmentVariables (0600 file), never ProgramArguments
// (world-readable via ps/launchctl print).
func splitSecretFlags(args []string) ([]string, map[string]string) {
	out := make([]string, 0, len(args))
	secrets := map[string]string{}
	expect := ""
	for _, a := range args {
		if expect != "" {
			secrets[expect] = a
			expect = ""
			continue
		}
		trimmed := strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
		if name, val, ok := strings.Cut(trimmed, "="); ok && secretFlags[name] {
			secrets[name] = val
			continue
		}
		if secretFlags[trimmed] {
			expect = trimmed
			continue
		}
		out = append(out, a)
	}
	return out, secrets
}

// secretFlags are daemon flags whose values must never appear in
// ProgramArguments or be echoed.
var secretFlags = map[string]bool{
	"auth-token": true,
}

// pathFlags are daemon flags whose values are filesystem paths. The
// service runs with WorkingDirectory ~/.manzanasd, not the installing
// shell's cwd, so relative values must be absolutized at install time.
var pathFlags = map[string]bool{
	"devices-config":       true,
	"device-mirror-socket": true,
	"state-dir":            true,
	"journal-dir":          true,
	"axe":                  true,
	"simbridge":            true,
	"lock-dir":             true,
}

// serviceArgs is the daemon argv the service runs with: the invocation's
// own flags minus --install-service itself, with path-valued flags
// resolved against the installing shell's cwd.
func serviceArgs(args []string) []string {
	abs := func(p string) string {
		if p == "" {
			return "" // an explicitly blank value keeps its "unset" meaning
		}
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	out := make([]string, 0, len(args))
	expectPath := false
	for _, a := range args {
		if expectPath {
			expectPath = false
			out = append(out, abs(a))
			continue
		}
		trimmed := strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
		if trimmed == "install-service" || strings.HasPrefix(trimmed, "install-service=") {
			continue
		}
		if name, val, ok := strings.Cut(trimmed, "="); ok && pathFlags[name] {
			out = append(out, a[:len(a)-len(val)]+abs(val))
			continue
		}
		if pathFlags[trimmed] {
			expectPath = true
		}
		out = append(out, a)
	}
	return out
}

// servicePATH is the PATH the LaunchAgent runs with: the installing
// shell's PATH (so tools like iproxy/pymobiledevice3 resolve under
// launchd, whose default PATH is bare) unioned with the launchd
// defaults, the Homebrew prefixes, and ~/bin.
func servicePATH(home string) string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		add(p)
	}
	for _, p := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin",
		"/usr/local/bin", "/opt/homebrew/bin", filepath.Join(home, "bin")} {
		add(p)
	}
	return strings.Join(out, string(os.PathListSeparator))
}

// pathEnvVars are the env fallbacks of the pathFlags: like the flags,
// relative values must be absolutized against the installing shell's
// cwd, since the service runs with WorkingDirectory ~/.manzanasd.
var pathEnvVars = map[string]bool{
	"MANZANASD_DEVICES_CONFIG":       true,
	"MANZANASD_DEVICE_MIRROR_SOCKET": true,
	"MANZANASD_STATE_DIR":            true,
	"MANZANASD_JOURNAL_DIR":          true,
	"MANZANASD_AXE":                  true,
	"MANZANASD_SIMBRIDGE":            true,
	"MANZANAS_LOCK_DIR":              true,
}

// serviceEnv snapshots the installing shell's MANZANASD_*/MANZANAS_*
// environment: every daemon flag has an env fallback, so dropping these
// would silently change the installed service's configuration (worst
// case, losing MANZANASD_AUTH_TOKEN and serving unauthenticated).
// Path-valued variables get the same cwd absolutization as pathFlags
// (empty values stay empty, keeping their "unset" meaning).
func serviceEnv() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(k, "MANZANASD_") || strings.HasPrefix(k, "MANZANAS_") {
			if pathEnvVars[k] && v != "" {
				if a, err := filepath.Abs(v); err == nil {
					v = a
				}
			}
			out[k] = v
		}
	}
	return out
}

// launchdPlist renders the LaunchAgent plist. Program arguments are
// XML-escaped so flag values with special characters survive the round
// trip.
func launchdPlist(label, binary string, args []string, logDir, workDir, path string, env map[string]string) string {
	var argv strings.Builder
	argv.WriteString("        <string>" + xmlEscape(binary) + "</string>\n")
	for _, a := range args {
		argv.WriteString("        <string>" + xmlEscape(a) + "</string>\n")
	}
	var envXML strings.Builder
	envXML.WriteString("        <key>PATH</key>\n        <string>" + xmlEscape(path) + "</string>\n")
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		envXML.WriteString("        <key>" + xmlEscape(k) + "</key>\n        <string>" + xmlEscape(env[k]) + "</string>\n")
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + xmlEscape(label) + `</string>
    <key>ProgramArguments</key>
    <array>
` + argv.String() + `    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <key>WorkingDirectory</key>
    <string>` + xmlEscape(workDir) + `</string>
    <key>EnvironmentVariables</key>
    <dict>
` + envXML.String() + `    </dict>
    <key>ProcessType</key>
    <string>Interactive</string>
    <key>StandardOutPath</key>
    <string>` + xmlEscape(filepath.Join(logDir, "manzanasd.out.log")) + `</string>
    <key>StandardErrorPath</key>
    <string>` + xmlEscape(filepath.Join(logDir, "manzanasd.err.log")) + `</string>
</dict>
</plist>
`
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
