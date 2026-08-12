package devices

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records commands and lets a test hook fabricate side effects
// (e.g. the clone creating the checkout, the build creating .xctestrun).
type fakeRunner struct {
	cmds   [][]string
	onRun  func(name string, args []string)
	failOn string
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.cmds = append(f.cmds, append([]string{name}, args...))
	if f.onRun != nil {
		f.onRun(name, args)
	}
	if f.failOn != "" && name == f.failOn {
		return []byte("boom"), os.ErrPermission
	}
	return nil, nil
}

func (f *fakeRunner) find(name string) []string {
	for _, c := range f.cmds {
		if c[0] == name {
			return c
		}
	}
	return nil
}

func newTestOnboarder(t *testing.T) (*Onboarder, *fakeRunner, string) {
	t.Helper()
	cache := t.TempDir()
	r := &fakeRunner{}
	return &Onboarder{Run: r.run, CacheDir: cache, Out: io.Discard}, r, cache
}

func makeWDACheckout(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "WebDriverAgent.xcodeproj"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func makeXCTestRun(t *testing.T, dd string) string {
	t.Helper()
	products := filepath.Join(dd, "Build", "Products")
	if err := os.MkdirAll(products, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(products, "WebDriverAgentRunner_iphoneos18.0-arm64.xctestrun")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOnboardBuildInvocation(t *testing.T) {
	ob, r, cache := newTestOnboarder(t)
	src := filepath.Join(cache, "src")
	makeWDACheckout(t, src)
	dd := filepath.Join(cache, "dd")
	want := makeXCTestRun(t, dd)

	res, err := ob.Onboard(context.Background(), OnboardOptions{
		UDID:        "UD1",
		WDASource:   src,
		Team:        "VSNK359D52",
		Keychain:    "dev-sign.keychain",
		ASCKeyPath:  "/k.p8",
		ASCKeyID:    "KEYID",
		ASCIssuerID: "ISSUER",
		DerivedData: dd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.XCTestRun != want {
		t.Errorf("XCTestRun = %s, want %s", res.XCTestRun, want)
	}
	build := r.find("xcodebuild")
	if build == nil {
		t.Fatal("xcodebuild never invoked")
	}
	joined := strings.Join(build, " ")
	for _, frag := range []string{
		"build-for-testing",
		"-scheme WebDriverAgentRunner",
		"-destination id=UD1",
		"-allowProvisioningUpdates",
		"-allowProvisioningDeviceRegistration",
		"-authenticationKeyPath /k.p8",
		"-authenticationKeyID KEYID",
		"-authenticationKeyIssuerID ISSUER",
		"PRODUCT_BUNDLE_IDENTIFIER=" + DefaultWDABundleID,
		"CODE_SIGN_STYLE=Automatic",
		"DEVELOPMENT_TEAM=VSNK359D52",
		"OTHER_CODE_SIGN_FLAGS=--keychain dev-sign.keychain",
	} {
		if !strings.Contains(joined, frag) {
			t.Errorf("xcodebuild args missing %q:\n%s", frag, joined)
		}
	}
	d := res.Config.WDA["UD1"]
	if !res.Config.Enabled || d.URL != "http://127.0.0.1:8100" ||
		d.Launch != "xctestrun:"+want || d.Forward != "8100:8100" {
		t.Errorf("config = %+v", res.Config)
	}
}

func TestOnboardCustomBundleAndForward(t *testing.T) {
	ob, r, cache := newTestOnboarder(t)
	src := filepath.Join(cache, "src")
	makeWDACheckout(t, src)
	dd := filepath.Join(cache, "dd")
	makeXCTestRun(t, dd)

	res, err := ob.Onboard(context.Background(), OnboardOptions{
		UDID: "UD1", WDASource: src, DerivedData: dd,
		BundleID: "com.example.wda", Forward: "9100:8100",
	})
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(r.find("xcodebuild"), " "); !strings.Contains(joined, "PRODUCT_BUNDLE_IDENTIFIER=com.example.wda") {
		t.Errorf("bundle id override missing: %s", joined)
	}
	d := res.Config.WDA["UD1"]
	if d.URL != "http://127.0.0.1:9100" || d.Forward != "9100:8100" {
		t.Errorf("forward wiring = %+v", d)
	}
}

func TestOnboardClonesWhenNoSource(t *testing.T) {
	ob, r, cache := newTestOnboarder(t)
	r.onRun = func(name string, args []string) {
		if name == "git" {
			makeWDACheckout(t, args[len(args)-1])
		}
	}
	dd := filepath.Join(cache, "dd")
	makeXCTestRun(t, dd)
	if _, err := ob.Onboard(context.Background(), OnboardOptions{UDID: "UD1", DerivedData: dd}); err != nil {
		t.Fatal(err)
	}
	clone := r.find("git")
	if clone == nil || clone[1] != "clone" || clone[len(clone)-1] != filepath.Join(cache, "wda") {
		t.Errorf("clone = %v", clone)
	}
}

func TestOnboardSkipBuild(t *testing.T) {
	ob, r, cache := newTestOnboarder(t)
	src := filepath.Join(cache, "src")
	makeWDACheckout(t, src)
	dd := filepath.Join(cache, "dd")
	makeXCTestRun(t, dd)
	if _, err := ob.Onboard(context.Background(), OnboardOptions{
		UDID: "UD1", WDASource: src, DerivedData: dd, SkipBuild: true,
	}); err != nil {
		t.Fatal(err)
	}
	if r.find("xcodebuild") != nil {
		t.Error("--skip-build still ran xcodebuild")
	}
}

func TestOnboardErrors(t *testing.T) {
	ob, r, cache := newTestOnboarder(t)
	src := filepath.Join(cache, "src")
	makeWDACheckout(t, src)
	dd := filepath.Join(cache, "dd")

	if _, err := ob.Onboard(context.Background(), OnboardOptions{}); err == nil {
		t.Error("empty UDID accepted")
	}
	if _, err := ob.Onboard(context.Background(), OnboardOptions{UDID: "UD1", WDASource: cache}); err == nil ||
		!strings.Contains(err.Error(), "WebDriverAgent.xcodeproj") {
		t.Errorf("bad source err = %v", err)
	}
	// Build "succeeds" but leaves no device .xctestrun (simulator build).
	if _, err := ob.Onboard(context.Background(), OnboardOptions{UDID: "UD1", WDASource: src, DerivedData: dd}); err == nil ||
		!strings.Contains(err.Error(), "no device .xctestrun") {
		t.Errorf("missing xctestrun err = %v", err)
	}
	r.failOn = "xcodebuild"
	if _, err := ob.Onboard(context.Background(), OnboardOptions{UDID: "UD1", WDASource: src, DerivedData: dd}); err == nil ||
		!strings.Contains(err.Error(), "build-for-testing failed") {
		t.Errorf("build failure err = %v", err)
	}
}
