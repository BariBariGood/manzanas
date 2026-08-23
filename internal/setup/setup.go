// Package setup implements `manzanas setup` — the guided, idempotent
// host-setup wizard: it installs/updates the GUI helpers to their stable
// canonical paths, detects which TCC grants are missing with the same
// live probes the doctor uses (never the TCC database, which lies both
// ways), walks the operator through granting each one, and re-verifies.
//
// The wizard is deliberately detect-and-instruct: it never triggers a
// TCC prompt, swaps a permissioned binary, or opens System Settings
// without an explicit confirmation, because a stray prompt or a binary
// swap costs a human round-trip re-toggling grants. All host effects go
// through the injectable Env so the flow is unit-testable on any OS.
package setup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
)

// MirrordIdentifier is the stable code-signing identifier every signed
// ~/bin/mirrord carries. TCC's designated requirement for an
// identity-signed binary is "identifier X and certificate leaf Y" — a
// rebuild signed with the same identifier and certificate satisfies the
// same requirement, so the grants survive the swap. (An ad-hoc
// signature's requirement is the cdhash of that exact build, which is
// why unsigned rebuilds keep losing grants.)
const MirrordIdentifier = "com.baribarigood.manzanasd.mirrord"

// LaunchAgent label for the mirrord helper.
const mirrordLabel = "com.baribarigood.manzanasd.mirrord"

// System Settings deep links (macOS 13+ "System Settings" honors the
// legacy x-apple.systempreferences scheme). Used when a TCC prompt will
// not reappear — macOS prompts once; after a decline only the pane works.
const (
	PaneAccessibility   = "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility"
	PaneScreenRecording = "x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture"
)

// Options are the `manzanas setup` flags.
type Options struct {
	// HelpersSrc is a repo checkout containing helpers/ (auto-detected
	// from the executable's directory and the working directory when
	// empty). Without one the wizard skips installs and only reports.
	HelpersSrc string
	// Socket is the mirrord unix socket (default ~/.manzanasd/mirrord.sock).
	Socket string
	// SignIdentity is the codesign identity for mirrord: "" auto-picks
	// the best identity in the keychain, "-" forces ad-hoc, anything
	// else is passed to codesign -s verbatim.
	SignIdentity string
	// Report skips every mutation: no builds, no installs, no Settings
	// windows, no extra prompt-triggering probe runs — detect current
	// state and print the checklist. The detection itself asks the
	// helper for a real capability probe, so on a host where the helper
	// has never been asked, macOS may still show its one-time
	// permission dialog.
	Report bool
	// Yes answers every confirmation with yes (still interactive for
	// the "press enter once granted" waits).
	Yes bool
}

// Env is every host effect the wizard performs, injectable for tests.
type Env struct {
	GOOS   string
	Home   string
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	stdin  *bufio.Reader // lazily wraps Stdin; shared so piped answers survive across prompts

	// RunOut runs a host command and returns its combined output.
	RunOut func(ctx context.Context, name string, args ...string) ([]byte, error)
	// FileExists reports whether path exists.
	FileExists func(path string) bool
	// MirrorDoctor runs the mirrord helper's live capability probes
	// over its unix socket.
	MirrorDoctor func(ctx context.Context, socket string) (mirror.DoctorReport, error)
	// Getenv reads an environment variable (os.Getenv).
	Getenv func(key string) string
	// Sleep pauses between probe retries (time.Sleep).
	Sleep func(d time.Duration)
}

// New returns an Env wired to the real host.
func New(stdout, stderr io.Writer, stdin io.Reader) *Env {
	home, _ := os.UserHomeDir()
	return &Env{
		GOOS:   runtimeGOOS,
		Home:   home,
		Stdout: stdout, Stderr: stderr, Stdin: stdin,
		RunOut: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		FileExists: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
		MirrorDoctor: func(ctx context.Context, socket string) (mirror.DoctorReport, error) {
			return mirror.New(socket).Doctor(ctx)
		},
		Getenv: os.Getenv,
		Sleep:  time.Sleep,
	}
}

// GrantStatus is one row of the final checklist.
type GrantStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"` // granted | missing | unknown
	Detail string `json:"detail"`
}

// Result is the wizard's machine-readable summary (--json).
type Result struct {
	OK     bool          `json:"ok"`
	Grants []GrantStatus `json:"grants"`
	Notes  []string      `json:"notes"`
}

// Run executes the wizard and returns the final checklist. A non-nil
// error is an operational failure; missing grants are reported in the
// Result, not as an error, so re-runs are cheap and idempotent.
func (e *Env) Run(ctx context.Context, opts Options) (Result, error) {
	// Helpers always install to ~/bin: install.sh hardcodes it, and it is
	// the stable canonical path the TCC grants are keyed to.
	binDir := filepath.Join(e.Home, "bin")
	if opts.Socket == "" {
		opts.Socket = filepath.Join(e.Home, ".manzanasd", "mirrord.sock")
	}

	var res Result
	out := e.Stdout
	fmt.Fprintln(out, "manzanas setup — guided host setup (idempotent; re-run any time)")
	fmt.Fprintln(out)

	if e.GOOS != "darwin" {
		return res, fmt.Errorf("setup configures a macOS host (helpers + TCC grants); this is %s — run it on the Mac, in a GUI terminal session", e.GOOS)
	}

	// ---- Step 1: helpers at stable canonical paths -----------------
	fmt.Fprintln(out, "== step 1/3: helpers at stable paths ==")
	src := e.findHelpersSrc(opts.HelpersSrc)
	if opts.HelpersSrc != "" && src == "" {
		return res, fmt.Errorf("--helpers-src %s does not contain helpers/mirrord/build.sh — not a manzanas checkout", opts.HelpersSrc)
	}
	mirrordBin := filepath.Join(binDir, "mirrord")
	probeAttempts := 1
	if opts.Report || src == "" {
		if src == "" && !opts.Report {
			note := "no repo checkout with helpers/ found — pass --helpers-src to install/update helpers; reporting current state only"
			res.Notes = append(res.Notes, note)
			fmt.Fprintln(out, "  "+note)
		}
		e.reportMirrordInstall(ctx, out, mirrordBin, &res)
	} else {
		if err := e.installMirrord(ctx, out, opts, src, mirrordBin, &res); err != nil {
			return res, err
		}
		e.installSimbridge(ctx, out, opts, src, binDir, &res)
		// install.sh returns as soon as launchctl bootstrap is issued;
		// the helper's socket may not be listening yet.
		probeAttempts = 6
	}
	fmt.Fprintln(out)

	// ---- Step 2: probe current grant state --------------------------
	fmt.Fprintln(out, "== step 2/3: detect TCC grants (live probes, not the TCC database) ==")
	grants := e.probeGrantsRetry(ctx, opts.Socket, mirrordBin, probeAttempts)
	renderGrants(out, grants)
	fmt.Fprintln(out)

	// ---- Step 3: walk missing grants ---------------------------------
	fmt.Fprintln(out, "== step 3/3: grant walkthrough ==")
	if opts.Report {
		fmt.Fprintln(out, "  --report: not prompting; instructions for anything missing are below")
		e.printMissingInstructions(out, grants, mirrordBin)
	} else {
		grants = e.walkGrants(ctx, out, opts, grants, mirrordBin, opts.Socket)
	}
	fmt.Fprintln(out)

	// ---- Checklist + the rebuild caveat ------------------------------
	fmt.Fprintln(out, "== checklist ==")
	renderGrants(out, grants)
	fmt.Fprintln(out)
	fmt.Fprintln(out, rebuildCaveat)
	res.Grants = grants
	// OK means verified-granted: an unknown grant is not a success, or a
	// fresh host whose helper is unreachable would exit green.
	res.OK = true
	for _, g := range grants {
		if g.Status != "granted" {
			res.OK = false
		}
	}
	return res, nil
}

const rebuildCaveat = `The rebuild caveat: macOS keys each TCC grant to the binary's code
signature, not just its path. A rebuild that changes the signature
silently invalidates the grants while System Settings keeps showing the
toggles on. What keeps grants across upgrades:
  - install helpers ONLY at their stable path (~/bin/mirrord) — never a
    temp build dir;
  - sign every build with the same identity + identifier (setup
    REQUIRES a stable identity unless ad-hoc is forced with
    --sign-identity -): the grant follows the identity and survives
    rebuilds;
  - with only an ad-hoc signature the grant dies with every swap:
    after upgrading, re-toggle Accessibility AND Screen & System Audio
    Recording OFF and back ON for ~/bin/mirrord in System Settings.
Verify after any upgrade with: manzanas doctor (probes, never guesses).`

// findHelpersSrc locates a checkout containing helpers/mirrord: the
// explicit flag, the working directory, or the executable's parents.
func (e *Env) findHelpersSrc(explicit string) string {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	} else {
		if wd, err := os.Getwd(); err == nil {
			candidates = append(candidates, wd)
		}
		if exe, err := os.Executable(); err == nil {
			d := filepath.Dir(exe)
			candidates = append(candidates, d, filepath.Dir(d))
		}
	}
	for _, c := range candidates {
		if e.FileExists(filepath.Join(c, "helpers", "mirrord", "build.sh")) {
			return c
		}
	}
	return ""
}

// reportMirrordInstall prints the current install state without touching it.
func (e *Env) reportMirrordInstall(ctx context.Context, out io.Writer, bin string, res *Result) {
	if !e.FileExists(bin) {
		fmt.Fprintf(out, "  mirrord: NOT installed at %s (helpers/mirrord/install.sh, or re-run setup with --helpers-src)\n", bin)
		return
	}
	id, cdhash, adhoc := e.signingInfo(ctx, bin)
	fmt.Fprintf(out, "  mirrord: installed at %s\n", bin)
	fmt.Fprintf(out, "    signature: %s\n", describeSignature(id, cdhash, adhoc))
	if adhoc {
		res.Notes = append(res.Notes, "mirrord is ad-hoc signed: every rebuild invalidates its TCC grants (re-toggle both after each swap); sign with a stable identity to fix")
	}
	if la := e.launchAgentState(ctx); la != "" {
		fmt.Fprintf(out, "    launchagent: %s\n", la)
	}
}

// installMirrord builds, signs, and (only when the signature actually
// changed, and only with confirmation) swaps ~/bin/mirrord via the
// canonical install.sh, which also loads the LaunchAgent.
func (e *Env) installMirrord(ctx context.Context, out io.Writer, opts Options, src, bin string, res *Result) error {
	buildSh := filepath.Join(src, "helpers", "mirrord", "build.sh")
	tmp, err := os.MkdirTemp("", "manzanas-setup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	fresh := filepath.Join(tmp, "mirrord")
	fmt.Fprintf(out, "  building mirrord from %s …\n", filepath.Dir(buildSh))
	// The build script signs ad-hoc here: setup applies its own signing
	// policy right below (hard identity requirement included), so the
	// script's identity check must not fail the build on hosts where
	// setup would go on to resolve or refuse an identity itself.
	if b, err := e.RunOut(ctx, "env", "MIRRORD_SIGN_IDENTITY=-", buildSh, fresh); err != nil {
		return fmt.Errorf("mirrord build failed: %v\n%s", err, firstLines(string(b), 12))
	}

	identity := e.pickIdentity(ctx, opts.SignIdentity)
	if identity == "-" {
		if opts.SignIdentity != "-" {
			return errors.New("no stable codesign identity in the keychain — an ad-hoc-signed mirrord loses its TCC grants on every rebuild.\n" +
				"Unlock the signing keychain (security unlock-keychain <keychain>) or pass --sign-identity;\n" +
				`--sign-identity - forces ad-hoc if you accept re-toggling both grants after every swap`)
		}
		fmt.Fprintln(out, "  signing: ad-hoc forced (--sign-identity -) — grants will NOT survive future swaps")
		res.Notes = append(res.Notes, "mirrord signed ad-hoc: re-toggle both grants after every future upgrade")
	} else {
		fmt.Fprintf(out, "  signing with %q (grants survive rebuilds signed with the same identity)\n", identity)
	}
	if b, err := e.RunOut(ctx, "codesign", "-f", "-s", identity,
		"--identifier", MirrordIdentifier, fresh); err != nil {
		return fmt.Errorf("codesign failed: %v\n%s", err, firstLines(string(b), 6))
	}

	if e.FileExists(bin) {
		_, oldHash, _ := e.signingInfo(ctx, bin)
		_, newHash, _ := e.signingInfo(ctx, fresh)
		if oldHash != "" && oldHash == newHash {
			fmt.Fprintf(out, "  mirrord at %s is already this exact build (cdhash %s) — leaving it, grants preserved\n", bin, oldHash)
			if !e.launchAgentLoaded(ctx) {
				fmt.Fprintln(out, "  LaunchAgent not loaded — bootstrapping it (same binary, grants preserved)")
				installSh := filepath.Join(src, "helpers", "mirrord", "install.sh")
				if b, err := e.RunOut(ctx, installSh, fresh); err != nil {
					return fmt.Errorf("mirrord LaunchAgent bootstrap failed: %v\n%s", err, firstLines(string(b), 12))
				}
			}
			return nil
		}
		fmt.Fprintf(out, "  a different mirrord is installed at %s\n", bin)
		// Grants follow the signing identity: a swap that changes the leaf
		// certificate drops them even though both builds are "signed".
		swapQ := "  swap it in? (same-identity swap keeps the grants)"
		if oldAuth, newAuth := e.signingAuthority(ctx, bin), e.signingAuthority(ctx, fresh); oldAuth != newAuth {
			swapQ = fmt.Sprintf("  swap it in? signing identity CHANGES (%q → %q) — this swap WILL drop the TCC grants (re-grant after)", oldAuth, newAuth)
		}
		if !e.confirm(opts, swapQ) {
			fmt.Fprintln(out, "  keeping the installed mirrord")
			res.Notes = append(res.Notes, "declined mirrord swap: installed helper left untouched")
			return nil
		}
	}
	installSh := filepath.Join(src, "helpers", "mirrord", "install.sh")
	if b, err := e.RunOut(ctx, installSh, fresh); err != nil {
		return fmt.Errorf("mirrord install failed: %v\n%s", err, firstLines(string(b), 12))
	}
	fmt.Fprintf(out, "  mirrord installed at %s; LaunchAgent %s loaded\n", bin, mirrordLabel)
	return nil
}

// installSimbridge builds the warm-actions helper when the AXe
// frameworks are present. simbridge holds no TCC grants, so this is a
// plain overwrite; missing frameworks is a note, not a failure.
func (e *Env) installSimbridge(ctx context.Context, out io.Writer, opts Options, src, binDir string, res *Result) {
	frameworks := e.Getenv("AXE_FRAMEWORKS")
	if frameworks == "" {
		frameworks = filepath.Join(e.Home, "axe", "Frameworks")
	}
	dst := filepath.Join(binDir, "simbridge")
	if !e.FileExists(frameworks) {
		fmt.Fprintf(out, "  simbridge: skipped (AXe frameworks not found at %s; set AXE_FRAMEWORKS)\n", frameworks)
		return
	}
	if !e.confirm(opts, fmt.Sprintf("  build+install simbridge to %s? (no TCC grants involved)", dst)) {
		return
	}
	buildSh := filepath.Join(src, "helpers", "simbridge", "build.sh")
	if b, err := e.RunOut(ctx, buildSh, dst); err != nil {
		fmt.Fprintf(out, "  simbridge build failed (non-fatal): %v\n%s\n", err, firstLines(string(b), 8))
		res.Notes = append(res.Notes, "simbridge build failed; warm actions unavailable until it is built (see docs/actions-warm.md)")
		return
	}
	fmt.Fprintf(out, "  simbridge installed at %s\n", dst)
}

// probeGrants maps the mirrord helper's live doctor probes onto the two
// TCC grants. "unknown" is an honest answer: an unreachable helper or a
// capture that could not be attempted proves nothing either way.
func (e *Env) probeGrants(ctx context.Context, socket, bin string) []GrantStatus {
	return e.probeGrantsRetry(ctx, socket, bin, 1)
}

// probeGrantsRetry retries an unreachable helper: right after an
// install the LaunchAgent may not be listening on its socket yet.
func (e *Env) probeGrantsRetry(ctx context.Context, socket, bin string, attempts int) []GrantStatus {
	acc := GrantStatus{Name: "accessibility", Status: "unknown"}
	scr := GrantStatus{Name: "screen_recording", Status: "unknown"}
	rep, err := e.MirrorDoctor(ctx, socket)
	for try := 1; err != nil && try < attempts; try++ {
		e.Sleep(500 * time.Millisecond)
		rep, err = e.MirrorDoctor(ctx, socket)
	}
	if err != nil {
		detail := "mirrord helper unreachable at " + socket + " — is the LaunchAgent loaded? (helpers/mirrord/install.sh)"
		acc.Detail, scr.Detail = detail, detail
		return []GrantStatus{acc, scr}
	}
	for _, c := range rep.Checks {
		switch c.Name {
		case "accessibility":
			acc.Detail = c.Detail
			if c.OK {
				acc.Status = "granted"
			} else {
				acc.Status = "missing"
			}
		case "capture":
			scr.Detail = c.Detail
			switch {
			case c.OK:
				scr.Status = "granted"
			case strings.Contains(c.Detail, "no phone window"):
				// Screen Recording could not be probed at all: the
				// mirroring window is absent, so a capture failure
				// says nothing about the grant.
				scr.Status = "unknown"
				scr.Detail = "not probed (no iPhone Mirroring window): open iPhone Mirroring, connect the phone, re-run"
			default:
				scr.Status = "missing"
			}
		}
	}
	return []GrantStatus{acc, scr}
}

// walkGrants handles each missing grant: trigger the prompt (once, on
// confirmation), deep-link the pane, wait, re-probe.
func (e *Env) walkGrants(ctx context.Context, out io.Writer, opts Options, grants []GrantStatus, bin, socket string) []GrantStatus {
	missing := missingNames(grants)
	if len(missing) == 0 {
		if allGranted(grants) {
			fmt.Fprintln(out, "  nothing to grant — every probed grant is in place")
		} else {
			// Nothing verifiably missing, but not everything verified
			// either — say so instead of claiming success.
			fmt.Fprintln(out, "  could not verify every grant — fix the conditions below and re-run:")
			e.printMissingInstructions(out, grants, bin)
		}
		return grants
	}
	fmt.Fprintf(out, "  missing: %s\n", strings.Join(missing, ", "))
	fmt.Fprintf(out, "  both grants go to %s (the helper binary — not manzanasd, not Terminal)\n", bin)

	// Over SSH a terminal-spawned probe attributes the TCC prompt to the
	// SSH session's responsible process (or shows nothing at all), so the
	// prompt-trigger step degrades to printed instructions.
	if e.overSSH() {
		fmt.Fprintln(out, "  SSH session detected — not triggering prompts (macOS would misattribute them); do this at the Mac's screen:")
		e.printMissingInstructions(out, grants, bin)
		return grants
	}

	// One probe run triggers the prompts for every missing grant, so ask
	// once. If a prompt was declined before, macOS will not re-prompt —
	// that is what the deep links are for.
	if e.confirm(opts, "  trigger the macOS permission prompt(s) now by running a real probe? (only do this at the Mac's screen)") {
		// The doctor exits non-zero whenever a probe fails — which is a
		// given here, since we only reach this with grants missing. Only
		// a failure to run the binary at all is an error.
		var exitErr *exec.ExitError
		if b, err := e.RunOut(ctx, bin, "--doctor"); err != nil && !errors.As(err, &exitErr) {
			fmt.Fprintf(out, "  probe run failed: %v\n%s\n", err, firstLines(string(b), 6))
		}
		fmt.Fprintln(out, "  if no prompt appeared, macOS already asked once — use the Settings pane below")
	}
	for _, g := range grants {
		if g.Status != "missing" {
			continue
		}
		pane, label := paneFor(g.Name)
		fmt.Fprintf(out, "  %s: System Settings ▸ Privacy & Security ▸ %s → add/enable %s\n", g.Name, label, bin)
		if e.confirm(opts, fmt.Sprintf("  open the %s pane now?", label)) {
			if _, err := e.RunOut(ctx, "open", pane); err != nil {
				fmt.Fprintf(out, "  could not open the pane (%v) — open System Settings manually\n", err)
			}
		}
	}
	for _, g := range grants {
		if g.Status == "unknown" {
			fmt.Fprintf(out, "  %s could not be verified: %s\n", g.Name, g.Detail)
		}
	}
	// Loop instead of a single one-shot re-probe: a premature enter (or
	// slow System Settings) should not force a full wizard re-run.
	after := grants
	for {
		fmt.Fprint(out, "  press enter once granted … ")
		_, readErr := e.reader().ReadString('\n')
		// TCC verdicts are read at process start: the long-running
		// LaunchAgent helper can keep seeing "denied" after the toggle
		// flips, so restart it before re-probing.
		fmt.Fprintln(out, "  restarting the helper (TCC is read at process start) and re-probing …")
		if _, err := e.RunOut(ctx, "launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", os.Getuid(), mirrordLabel)); err != nil {
			fmt.Fprintf(out, "  kickstart failed (%v) — re-probing anyway\n", err)
		}
		after = e.probeGrantsRetry(ctx, socket, bin, 6)
		if len(missingNames(after)) == 0 {
			return after
		}
		fmt.Fprintf(out, "  still missing: %s\n", strings.Join(missingNames(after), ", "))
		fmt.Fprintln(out, "  If the Settings toggle is ON but the grant is dead (stale row after a signature change):")
		fmt.Fprintln(out, "  remove the entry with −, re-add the binary via + → ⌘⇧G, re-enable. As a last resort:")
		fmt.Fprintf(out, "    tccutil reset Accessibility %s ; tccutil reset ScreenCapture %s\n", MirrordIdentifier, MirrordIdentifier)
		// The continue decision must be a real interactive answer: with
		// --yes or a closed stdin an auto-yes would loop (and kickstart
		// the helper) forever while the grant stays missing.
		if readErr != nil || !e.confirm(Options{}, "  re-probe again?") {
			return after
		}
	}
}

// overSSH reports whether this process is running inside an SSH session.
func (e *Env) overSSH() bool {
	return e.Getenv("SSH_CONNECTION") != "" || e.Getenv("SSH_CLIENT") != "" || e.Getenv("SSH_TTY") != ""
}

// printMissingInstructions is the --report replacement for walkGrants:
// the same instructions, zero side effects.
func (e *Env) printMissingInstructions(out io.Writer, grants []GrantStatus, bin string) {
	for _, g := range grants {
		if g.Status == "granted" {
			continue
		}
		pane, label := paneFor(g.Name)
		fmt.Fprintf(out, "  %s (%s):\n", g.Name, g.Status)
		fmt.Fprintf(out, "    grant to: %s\n", bin)
		fmt.Fprintf(out, "    pane:     System Settings ▸ Privacy & Security ▸ %s\n", label)
		fmt.Fprintf(out, "    deeplink: open %q\n", pane)
		fmt.Fprintf(out, "    prompt:   %s --doctor  (a real probe; run it in a GUI terminal ON the Mac)\n", bin)
	}
}

func paneFor(grant string) (deeplink, label string) {
	switch grant {
	case "screen_recording":
		return PaneScreenRecording, "Screen & System Audio Recording"
	default:
		return PaneAccessibility, "Accessibility"
	}
}

func allGranted(grants []GrantStatus) bool {
	for _, g := range grants {
		if g.Status != "granted" {
			return false
		}
	}
	return true
}

func missingNames(grants []GrantStatus) []string {
	var out []string
	for _, g := range grants {
		if g.Status == "missing" {
			out = append(out, g.Name)
		}
	}
	return out
}

func renderGrants(out io.Writer, grants []GrantStatus) {
	for _, g := range grants {
		mark := map[string]string{"granted": "ok  ", "missing": "MISS", "unknown": "?   "}[g.Status]
		fmt.Fprintf(out, "  [%s] %-17s %s\n", mark, g.Name, g.Detail)
	}
}

// confirm asks a y/N question; --yes short-circuits to yes.
func (e *Env) confirm(opts Options, q string) bool {
	if opts.Yes {
		return true
	}
	fmt.Fprintf(e.Stdout, "%s [y/N] ", q)
	line, _ := e.reader().ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

// reader returns the shared buffered stdin reader; a fresh bufio.Reader
// per prompt would discard read-ahead and drop piped answers.
func (e *Env) reader() *bufio.Reader {
	if e.stdin == nil {
		e.stdin = bufio.NewReader(e.Stdin)
	}
	return e.stdin
}

var cdhashRe = regexp.MustCompile(`(?m)^CDHash=([0-9a-f]+)`)
var identRe = regexp.MustCompile(`(?m)^Identifier=(\S+)`)

var authorityRe = regexp.MustCompile(`(?m)^Authority=(.+)$`)

// signingAuthority reads a binary's leaf signing certificate (the first
// Authority= line of codesign -dvvv); empty for unsigned, "(ad-hoc)"
// for ad-hoc signatures.
func (e *Env) signingAuthority(ctx context.Context, bin string) string {
	b, err := e.RunOut(ctx, "codesign", "-dvvv", bin)
	if err != nil {
		return ""
	}
	if strings.Contains(string(b), "Signature=adhoc") {
		return "(ad-hoc)"
	}
	if m := authorityRe.FindStringSubmatch(string(b)); m != nil {
		return m[1]
	}
	return ""
}

// signingInfo reads a binary's signing identifier, cdhash, and whether
// the signature is ad-hoc (codesign -dvvv writes to stderr; RunOut
// captures combined output).
func (e *Env) signingInfo(ctx context.Context, bin string) (identifier, cdhash string, adhoc bool) {
	b, err := e.RunOut(ctx, "codesign", "-dvvv", bin)
	if err != nil {
		return "", "", false
	}
	s := string(b)
	if m := identRe.FindStringSubmatch(s); m != nil {
		identifier = m[1]
	}
	if m := cdhashRe.FindStringSubmatch(s); m != nil {
		cdhash = m[1]
	}
	adhoc = strings.Contains(s, "Signature=adhoc")
	return identifier, cdhash, adhoc
}

func describeSignature(identifier, cdhash string, adhoc bool) string {
	if cdhash == "" {
		return "unsigned or unreadable"
	}
	kind := "identity-signed (grants survive same-identity rebuilds)"
	if adhoc {
		kind = "ad-hoc (grants die with every swap)"
	}
	return fmt.Sprintf("%s, identifier %s, cdhash %s", kind, identifier, cdhash)
}

// pickIdentity resolves the codesign identity: the explicit flag, else
// the best identity in the keychain (Developer ID Application, then
// Apple Development, then Apple Distribution), else ad-hoc ("-").
func (e *Env) pickIdentity(ctx context.Context, explicit string) string {
	if explicit != "" {
		return explicit
	}
	b, err := e.RunOut(ctx, "security", "find-identity", "-v", "-p", "codesigning")
	if err != nil {
		return "-"
	}
	names := parseIdentities(string(b))
	for _, prefix := range []string{"Developer ID Application:", "Apple Development:", "Apple Distribution:"} {
		for _, n := range names {
			if strings.HasPrefix(n, prefix) {
				return n
			}
		}
	}
	return "-"
}

var identityLineRe = regexp.MustCompile(`\d+\)\s+[0-9A-F]+\s+"([^"]+)"`)

func parseIdentities(out string) []string {
	var names []string
	for _, m := range identityLineRe.FindAllStringSubmatch(out, -1) {
		names = append(names, m[1])
	}
	return names
}

// launchAgentLoaded reports whether the mirrord LaunchAgent is known
// to launchd for this GUI session.
func (e *Env) launchAgentLoaded(ctx context.Context) bool {
	_, err := e.RunOut(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), mirrordLabel))
	return err == nil
}

// launchAgentState summarizes the mirrord LaunchAgent's launchctl state.
func (e *Env) launchAgentState(ctx context.Context) string {
	b, err := e.RunOut(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), mirrordLabel))
	if err != nil {
		return "not loaded (helpers/mirrord/install.sh loads it)"
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "state = ") {
			return strings.TrimPrefix(line, "state = ")
		}
	}
	return "loaded"
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
