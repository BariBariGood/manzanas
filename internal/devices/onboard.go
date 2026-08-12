package devices

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BariBariGood/manzanas/internal/actions/wda"
	"github.com/BariBariGood/manzanas/proto"
)

// DefaultWDABundleID is the manzanas-owned bundle id WDA is built under,
// so the build never trips over the WDA project's hardcoded upstream team
// ("No Account for Team ...") — see docs/devices.md.
const DefaultWDABundleID = "com.manzanas.wda"

// wdaRepoURL is the upstream WebDriverAgent source fetched when no local
// checkout is given or cached.
const wdaRepoURL = "https://github.com/appium/WebDriverAgent"

// OnboardOptions configures a device onboarding run (`manzanas device
// onboard <udid>`): build WDA for the device with a manzanas-owned bundle
// id, sign it headlessly, and produce the daemon config that supervises
// the runner and the usbmux forward.
type OnboardOptions struct {
	UDID string
	// WDASource is a WebDriverAgent checkout (contains
	// WebDriverAgent.xcodeproj); empty means use/clone the cached copy
	// under <cache>/wda.
	WDASource string
	// BundleID overrides DefaultWDABundleID.
	BundleID string
	// Team is the Apple Development team id (DEVELOPMENT_TEAM); empty
	// lets automatic signing pick the account's team.
	Team string
	// Keychain names a dedicated signing keychain for headless codesign
	// over SSH (OTHER_CODE_SIGN_FLAGS=--keychain <name>); the keychain
	// must be unlocked and its key partition list must include codesign.
	Keychain string
	// ASCKeyPath/ASCKeyID/ASCIssuerID are App Store Connect API
	// credentials for headless automatic provisioning
	// (-authenticationKeyPath/-authenticationKeyID/
	// -authenticationKeyIssuerID); without them profile creation needs a
	// logged-in Xcode account.
	ASCKeyPath  string
	ASCKeyID    string
	ASCIssuerID string
	// Forward is the usbmux port pair "<local>:<remote>" (default
	// 8100:8100) the daemon supervises alongside the runner.
	Forward string
	// DerivedData overrides the xcodebuild derived-data dir (default
	// <cache>/wda-derived-<udid>).
	DerivedData string
	// SkipBuild reuses an existing .xctestrun from DerivedData instead
	// of rebuilding (config-regeneration runs).
	SkipBuild bool
}

// OnboardResult is what an onboarding run produced: the device build's
// .xctestrun and the single-device daemon config wired to it.
type OnboardResult struct {
	XCTestRun string
	Config    proto.DevicesConfig
}

// Onboarder runs the onboarding steps; command execution and filesystem
// probes are injectable so the plumbing is unit-testable off-Mac.
type Onboarder struct {
	// Run executes one command to completion, returning combined output.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// CacheDir roots cached WDA checkouts and derived data (default
	// ~/.manzanas).
	CacheDir string
	// Out receives progress lines (default io.Discard).
	Out io.Writer
}

// NewOnboarder builds an Onboarder with real command execution.
func NewOnboarder(out io.Writer) *Onboarder {
	if out == nil {
		out = io.Discard
	}
	return &Onboarder{Run: runCombined, Out: out}
}

func runCombined(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func (o *Onboarder) cacheDir() (string, error) {
	if o.CacheDir != "" {
		return o.CacheDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".manzanas"), nil
}

func (o *Onboarder) progress(format string, args ...any) {
	fmt.Fprintf(o.Out, format+"\n", args...)
}

// Onboard runs the full flow: locate/fetch WDA source, build-for-testing
// for the device under a manzanas-owned bundle id with automatic signing,
// find the produced .xctestrun, and assemble the daemon config (URL +
// xctestrun launch spec + supervised forward).
func (o *Onboarder) Onboard(ctx context.Context, opts OnboardOptions) (OnboardResult, error) {
	var res OnboardResult
	if opts.UDID == "" {
		return res, fmt.Errorf("onboard: device UDID required")
	}
	if opts.BundleID == "" {
		opts.BundleID = DefaultWDABundleID
	}
	if opts.Forward == "" {
		opts.Forward = "8100:8100"
	}
	// Same validation the daemon applies, so onboarding never emits a
	// config the daemon would refuse.
	fwd, err := wda.ParseForward(opts.UDID, opts.Forward)
	if err != nil {
		return res, fmt.Errorf("onboard: invalid --forward: %w", err)
	}

	dd := opts.DerivedData
	if dd == "" {
		cache, err := o.cacheDir()
		if err != nil {
			return res, err
		}
		dd = filepath.Join(cache, "wda-derived-"+opts.UDID)
	}

	if !opts.SkipBuild {
		src, err := o.resolveSource(ctx, opts.WDASource)
		if err != nil {
			return res, err
		}
		if err := o.build(ctx, src, dd, opts); err != nil {
			return res, err
		}
	}

	xctestrun, err := findXCTestRun(dd)
	if err != nil {
		return res, err
	}
	res.XCTestRun = xctestrun
	res.Config = proto.DevicesConfig{
		Enabled: true,
		WDA: map[string]proto.DeviceWDAConfig{
			opts.UDID: {
				URL:     fmt.Sprintf("http://127.0.0.1:%d", fwd.Local),
				Launch:  "xctestrun:" + xctestrun,
				Forward: opts.Forward,
			},
		},
	}
	return res, nil
}

// resolveSource returns a WebDriverAgent checkout: the given path, an
// existing cached clone, or a fresh shallow clone of upstream WDA.
func (o *Onboarder) resolveSource(ctx context.Context, given string) (string, error) {
	if given != "" {
		if !hasXcodeproj(given) {
			return "", fmt.Errorf("onboard: %s does not contain WebDriverAgent.xcodeproj", given)
		}
		return given, nil
	}
	cache, err := o.cacheDir()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(cache, "wda")
	if hasXcodeproj(dst) {
		o.progress("using cached WebDriverAgent checkout at %s", dst)
		return dst, nil
	}
	o.progress("cloning %s into %s", wdaRepoURL, dst)
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return "", err
	}
	// dst without an xcodeproj is a leftover from an interrupted clone;
	// git refuses a non-empty destination, so self-heal by removing it.
	if _, err := os.Stat(dst); err == nil {
		o.progress("removing incomplete checkout at %s", dst)
		if err := os.RemoveAll(dst); err != nil {
			return "", fmt.Errorf("onboard: remove incomplete checkout %s: %w", dst, err)
		}
	}
	if out, err := o.Run(ctx, "git", "clone", "--depth", "1", wdaRepoURL, dst); err != nil {
		return "", fmt.Errorf("onboard: clone WebDriverAgent into %s: %w: %s", dst, err, firstLine(out))
	}
	if !hasXcodeproj(dst) {
		return "", fmt.Errorf("onboard: clone of %s has no WebDriverAgent.xcodeproj", wdaRepoURL)
	}
	return dst, nil
}

func hasXcodeproj(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "WebDriverAgent.xcodeproj"))
	return err == nil
}

// build runs `xcodebuild build-for-testing` for the device with the
// manzanas-owned bundle id and automatic signing. The overrides exist
// because the WDA project ships a hardcoded upstream team + bundle id
// that fail immediately for anyone else; the ASC API flags let automatic
// provisioning create the *.xctrunner profile headlessly (no logged-in
// Xcode); the keychain flag makes codesign work over SSH.
func (o *Onboarder) build(ctx context.Context, src, dd string, opts OnboardOptions) error {
	args := []string{
		"build-for-testing",
		"-project", filepath.Join(src, "WebDriverAgent.xcodeproj"),
		"-scheme", "WebDriverAgentRunner",
		"-destination", "id=" + opts.UDID,
		"-derivedDataPath", dd,
		"-allowProvisioningUpdates",
		"-allowProvisioningDeviceRegistration",
	}
	if opts.ASCKeyPath != "" {
		args = append(args,
			"-authenticationKeyPath", opts.ASCKeyPath,
			"-authenticationKeyID", opts.ASCKeyID,
			"-authenticationKeyIssuerID", opts.ASCIssuerID,
		)
	}
	args = append(args,
		"PRODUCT_BUNDLE_IDENTIFIER="+opts.BundleID,
		"CODE_SIGN_STYLE=Automatic",
	)
	if opts.Team != "" {
		args = append(args, "DEVELOPMENT_TEAM="+opts.Team)
	}
	if opts.Keychain != "" {
		args = append(args, "OTHER_CODE_SIGN_FLAGS=--keychain "+opts.Keychain)
	}
	o.progress("building WDA for %s (bundle id %s)…", opts.UDID, opts.BundleID)
	out, err := o.Run(ctx, "xcodebuild", args...)
	if err != nil {
		return fmt.Errorf("onboard: xcodebuild build-for-testing failed: %w\n%s", err, tail(out, 30))
	}
	return nil
}

// findXCTestRun locates the device .xctestrun produced by the build
// (Build/Products/WebDriverAgentRunner_iphoneos*.xctestrun; simulator
// xctestruns are skipped). Newest wins when several Xcode versions left
// their own.
func findXCTestRun(dd string) (string, error) {
	products := filepath.Join(dd, "Build", "Products")
	matches, _ := filepath.Glob(filepath.Join(products, "WebDriverAgentRunner_iphoneos*.xctestrun"))
	if len(matches) == 0 {
		return "", fmt.Errorf("onboard: no device .xctestrun under %s (a simulator-only build?)", products)
	}
	sort.Slice(matches, func(i, j int) bool {
		fi, ei := os.Stat(matches[i])
		fj, ej := os.Stat(matches[j])
		if ei != nil || ej != nil {
			return matches[i] > matches[j]
		}
		return fi.ModTime().After(fj.ModTime())
	})
	return matches[0], nil
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(s)
}

// tail returns the last n lines of command output (xcodebuild failures
// bury the actionable error at the bottom).
func tail(b []byte, n int) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
