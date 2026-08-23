package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
)

// testEnv returns an Env faked onto a darwin host with an installed,
// identity-signed mirrord and both grants present. Tests override
// pieces to exercise each path.
func testEnv(t *testing.T) (*Env, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	e := &Env{
		GOOS:   "darwin",
		Home:   "/Users/tt",
		Stdout: out, Stderr: out, Stdin: strings.NewReader("\n"),
		FileExists: func(path string) bool { return path == "/Users/tt/bin/mirrord" },
		Getenv:     func(string) string { return "" },
		Sleep:      func(time.Duration) {},
		RunOut: func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch name {
			case "codesign":
				return []byte("Identifier=" + MirrordIdentifier + "\nCDHash=abc123\nAuthority=Apple Development: T T\n"), nil
			case "launchctl":
				return []byte("\tstate = running\n"), nil
			}
			return nil, fmt.Errorf("unexpected command %s %v", name, args)
		},
		MirrorDoctor: func(context.Context, string) (mirror.DoctorReport, error) {
			return mirror.DoctorReport{OK: true, Checks: []mirror.DoctorCheck{
				{Name: "accessibility", OK: true, Detail: "granted"},
				{Name: "capture", OK: true, Detail: "captured 100x200"},
			}}, nil
		},
	}
	return e, out
}

func TestReportAllGranted(t *testing.T) {
	e, out := testEnv(t)
	res, err := e.Run(context.Background(), Options{Report: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("want OK, got %+v", res)
	}
	for _, g := range res.Grants {
		if g.Status != "granted" {
			t.Errorf("grant %s = %s, want granted", g.Name, g.Status)
		}
	}
	if !strings.Contains(out.String(), "identity-signed") {
		t.Errorf("output should describe the signature:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "rebuild caveat") {
		t.Errorf("output should explain the rebuild caveat:\n%s", out.String())
	}
}

func TestReportMissingGrantsPrintsDeepLinks(t *testing.T) {
	e, out := testEnv(t)
	e.MirrorDoctor = func(context.Context, string) (mirror.DoctorReport, error) {
		return mirror.DoctorReport{Checks: []mirror.DoctorCheck{
			{Name: "accessibility", OK: false, Detail: "MISSING"},
			{Name: "capture", OK: false, Detail: "capture failed: permission"},
		}}, nil
	}
	res, err := e.Run(context.Background(), Options{Report: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("want not OK with missing grants")
	}
	s := out.String()
	for _, want := range []string{PaneAccessibility, PaneScreenRecording, "Screen & System Audio Recording"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

func TestNoPhoneWindowIsUnknownNotMissing(t *testing.T) {
	e, _ := testEnv(t)
	e.MirrorDoctor = func(context.Context, string) (mirror.DoctorReport, error) {
		return mirror.DoctorReport{Checks: []mirror.DoctorCheck{
			{Name: "accessibility", OK: true, Detail: "granted"},
			{Name: "capture", OK: false, Detail: "no phone window found"},
		}}, nil
	}
	res, err := e.Run(context.Background(), Options{Report: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("an unverified grant must not report OK")
	}
	if res.Grants[1].Status != "unknown" {
		t.Fatalf("screen_recording = %s, want unknown", res.Grants[1].Status)
	}
}

func TestHelperUnreachableIsUnknown(t *testing.T) {
	e, out := testEnv(t)
	e.MirrorDoctor = func(context.Context, string) (mirror.DoctorReport, error) {
		return mirror.DoctorReport{}, errors.New("dial unix: no such file")
	}
	res, err := e.Run(context.Background(), Options{Report: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("an unreachable helper must not report OK")
	}
	for _, g := range res.Grants {
		if g.Status != "unknown" {
			t.Errorf("grant %s = %s, want unknown (not missing)", g.Name, g.Status)
		}
	}
	if !strings.Contains(out.String(), "unreachable") {
		t.Errorf("output should say the helper is unreachable:\n%s", out.String())
	}
}

func TestNonDarwinFails(t *testing.T) {
	e, _ := testEnv(t)
	e.GOOS = "linux"
	if _, err := e.Run(context.Background(), Options{Report: true}); err == nil {
		t.Fatal("want error on non-darwin host")
	}
}

func TestWalkDeclinedPromptStillRenders(t *testing.T) {
	e, out := testEnv(t)
	// Missing accessibility; user answers "n" to both confirmations,
	// then enter at the wait. Nothing may be executed besides probes.
	e.Stdin = strings.NewReader("n\nn\n\n")
	e.MirrorDoctor = func(context.Context, string) (mirror.DoctorReport, error) {
		return mirror.DoctorReport{Checks: []mirror.DoctorCheck{
			{Name: "accessibility", OK: false, Detail: "MISSING"},
			{Name: "capture", OK: true, Detail: "ok"},
		}}, nil
	}
	ran := []string{}
	base := e.RunOut
	e.RunOut = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name)
		return base(ctx, name, args...)
	}
	res, err := e.Run(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("want not OK while accessibility is missing")
	}
	for _, name := range ran {
		if name == "open" || strings.HasSuffix(name, "mirrord") {
			t.Errorf("declined confirmation must not run %q", name)
		}
	}
	if !strings.Contains(out.String(), "Privacy & Security") {
		t.Errorf("output should point at System Settings:\n%s", out.String())
	}
}

func TestPipedAnswersSurviveAcrossPrompts(t *testing.T) {
	e, _ := testEnv(t)
	// Decline the prompt trigger, accept opening the pane, then enter at
	// the wait: the second answer must not be swallowed by the first read.
	e.Stdin = strings.NewReader("n\ny\n\n")
	e.MirrorDoctor = func(context.Context, string) (mirror.DoctorReport, error) {
		return mirror.DoctorReport{Checks: []mirror.DoctorCheck{
			{Name: "accessibility", OK: false, Detail: "MISSING"},
			{Name: "capture", OK: true, Detail: "ok"},
		}}, nil
	}
	opened := false
	base := e.RunOut
	e.RunOut = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "open" {
			opened = true
			return nil, nil
		}
		return base(ctx, name, args...)
	}
	if _, err := e.Run(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if !opened {
		t.Fatal("second piped answer (y) was dropped — the Settings pane was never opened")
	}
}

func TestSSHSessionSkipsPromptTrigger(t *testing.T) {
	e, out := testEnv(t)
	e.Getenv = func(key string) string {
		if key == "SSH_CONNECTION" {
			return "10.0.0.1 1 10.0.0.2 22"
		}
		return ""
	}
	e.MirrorDoctor = func(context.Context, string) (mirror.DoctorReport, error) {
		return mirror.DoctorReport{Checks: []mirror.DoctorCheck{
			{Name: "accessibility", OK: false, Detail: "MISSING"},
			{Name: "capture", OK: true, Detail: "ok"},
		}}, nil
	}
	ran := []string{}
	base := e.RunOut
	e.RunOut = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name)
		return base(ctx, name, args...)
	}
	// --yes would auto-confirm the prompt trigger; SSH must override it.
	res, err := e.Run(context.Background(), Options{Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("want not OK while accessibility is missing")
	}
	for _, name := range ran {
		if name == "open" || strings.HasSuffix(name, "mirrord") {
			t.Errorf("SSH session must not run %q", name)
		}
	}
	if !strings.Contains(out.String(), "SSH session detected") {
		t.Errorf("output should explain the SSH downgrade:\n%s", out.String())
	}
}

func TestYesWithMissingGrantTerminates(t *testing.T) {
	// --yes must not auto-answer the "re-probe again?" question: with a
	// grant stuck missing and stdin at EOF that would loop forever.
	e, _ := testEnv(t)
	e.Stdin = strings.NewReader("") // closed stdin, like a scripted run
	e.MirrorDoctor = func(context.Context, string) (mirror.DoctorReport, error) {
		return mirror.DoctorReport{Checks: []mirror.DoctorCheck{
			{Name: "accessibility", OK: false, Detail: "MISSING"},
			{Name: "capture", OK: true, Detail: "ok"},
		}}, nil
	}
	done := make(chan error, 1)
	go func() {
		res, err := e.Run(context.Background(), Options{Yes: true})
		if err == nil && res.OK {
			err = errors.New("want not OK with a missing grant")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("setup --yes hung in the re-probe loop")
	}
}

func TestWalkKickstartsHelperBeforeReprobe(t *testing.T) {
	e, _ := testEnv(t)
	e.Stdin = strings.NewReader("n\nn\n\n")
	e.MirrorDoctor = func(context.Context, string) (mirror.DoctorReport, error) {
		return mirror.DoctorReport{Checks: []mirror.DoctorCheck{
			{Name: "accessibility", OK: false, Detail: "MISSING"},
			{Name: "capture", OK: true, Detail: "ok"},
		}}, nil
	}
	kickstarted := false
	base := e.RunOut
	e.RunOut = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "launchctl" && len(args) > 0 && args[0] == "kickstart" {
			kickstarted = true
			return nil, nil
		}
		return base(ctx, name, args...)
	}
	if _, err := e.Run(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if !kickstarted {
		t.Fatal("the helper must be kickstarted before the re-probe (TCC is read at process start)")
	}
}

func TestFindHelpersSrcExplicit(t *testing.T) {
	e, _ := testEnv(t)
	e.FileExists = func(path string) bool {
		return path == "/src/helpers/mirrord/build.sh"
	}
	if got := e.findHelpersSrc("/src"); got != "/src" {
		t.Fatalf("findHelpersSrc = %q, want /src", got)
	}
	if got := e.findHelpersSrc("/elsewhere"); got != "" {
		t.Fatalf("findHelpersSrc = %q, want empty", got)
	}
}

func TestExplicitBadHelpersSrcIsAnError(t *testing.T) {
	e, _ := testEnv(t)
	_, err := e.Run(context.Background(), Options{HelpersSrc: "/typo"})
	if err == nil || !strings.Contains(err.Error(), "/typo") {
		t.Fatalf("want error naming the bad --helpers-src, got %v", err)
	}
}

// installEnv fakes a checkout at /src plus an installed mirrord, with
// scriptable identity lookup and command recording.
func installEnv(t *testing.T, identities string) (*Env, *[]string) {
	e, _ := testEnv(t)
	e.FileExists = func(path string) bool {
		return path == "/src/helpers/mirrord/build.sh" || path == "/Users/tt/bin/mirrord"
	}
	ran := &[]string{}
	e.RunOut = func(_ context.Context, name string, args ...string) ([]byte, error) {
		*ran = append(*ran, name+" "+strings.Join(args, " "))
		switch name {
		case "security":
			if identities == "" {
				return nil, errors.New("no identities")
			}
			return []byte(identities), nil
		case "codesign":
			return []byte("Identifier=" + MirrordIdentifier + "\nCDHash=abc123\nAuthority=Apple Development: T T\n"), nil
		case "launchctl":
			return nil, errors.New("not loaded")
		}
		return nil, nil
	}
	return e, ran
}

func TestNoIdentityIsAHardError(t *testing.T) {
	e, _ := installEnv(t, "")
	var res Result
	err := e.installMirrord(context.Background(), e.Stdout, Options{}, "/src", "/Users/tt/bin/mirrord", &res)
	if err == nil || !strings.Contains(err.Error(), "no stable codesign identity") {
		t.Fatalf("unsigned install must fail hard, got %v", err)
	}
}

func TestExplicitAdhocIsAllowed(t *testing.T) {
	e, _ := installEnv(t, "")
	var res Result
	if err := e.installMirrord(context.Background(), e.Stdout, Options{SignIdentity: "-", Yes: true}, "/src", "/Users/tt/bin/mirrord", &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "ad-hoc") {
		t.Fatalf("forced ad-hoc must carry the re-toggle note, got %v", res.Notes)
	}
}

func TestBuildScriptGetsAdhocOverride(t *testing.T) {
	// Setup applies its own signing policy after the build, so the build
	// script's hard identity requirement must be bypassed — otherwise
	// --sign-identity - could never reach setup's own fallback.
	e, ran := installEnv(t, `  1) AAA "Apple Development: X (1)"`)
	var res Result
	if err := e.installMirrord(context.Background(), e.Stdout, Options{}, "/src", "/Users/tt/bin/mirrord", &res); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cmd := range *ran {
		if strings.HasPrefix(cmd, "env MIRRORD_SIGN_IDENTITY=- ") && strings.Contains(cmd, "build.sh") {
			found = true
		}
	}
	if !found {
		t.Fatalf("build.sh must be invoked with MIRRORD_SIGN_IDENTITY=-, ran: %v", *ran)
	}
}

func TestSameCdhashStillBootstrapsUnloadedAgent(t *testing.T) {
	e, ran := installEnv(t, `  1) AAA "Apple Development: X (1)"`)
	var res Result
	if err := e.installMirrord(context.Background(), e.Stdout, Options{}, "/src", "/Users/tt/bin/mirrord", &res); err != nil {
		t.Fatal(err)
	}
	installed := false
	for _, cmd := range *ran {
		if strings.Contains(cmd, "install.sh") {
			installed = true
		}
	}
	if !installed {
		t.Fatalf("same-cdhash path with an unloaded LaunchAgent must run install.sh, ran: %v", *ran)
	}
}

func TestParseIdentities(t *testing.T) {
	out := `  1) ABCDEF0123456789 "Developer ID Application: Ivan Del Rio (VSNK359D52)"
  2) 123456ABCDEF0123 "Apple Development: Ivan Del Rio (ABC123)"
     2 valid identities found`
	names := parseIdentities(out)
	if len(names) != 2 || !strings.HasPrefix(names[0], "Developer ID Application:") {
		t.Fatalf("parseIdentities = %v", names)
	}
}

func TestPickIdentityPrefersDeveloperID(t *testing.T) {
	e, _ := testEnv(t)
	e.RunOut = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "security" {
			return []byte(`  1) AAA "Apple Development: X (1)"
  2) BBB "Developer ID Application: X (2)"`), nil
		}
		return nil, errors.New("unexpected")
	}
	if got := e.pickIdentity(context.Background(), ""); got != "Developer ID Application: X (2)" {
		t.Fatalf("pickIdentity = %q", got)
	}
	if got := e.pickIdentity(context.Background(), "-"); got != "-" {
		t.Fatalf("explicit ad-hoc = %q", got)
	}
	e.RunOut = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no keychain") }
	if got := e.pickIdentity(context.Background(), ""); got != "-" {
		t.Fatalf("no keychain should fall back to ad-hoc, got %q", got)
	}
}

func TestSigningInfo(t *testing.T) {
	e, _ := testEnv(t)
	e.RunOut = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("Identifier=mirrord\nCDHash=deadbeef\nSignature=adhoc\n"), nil
	}
	id, hash, adhoc := e.signingInfo(context.Background(), "/x")
	if id != "mirrord" || hash != "deadbeef" || !adhoc {
		t.Fatalf("signingInfo = %q %q %v", id, hash, adhoc)
	}
}
