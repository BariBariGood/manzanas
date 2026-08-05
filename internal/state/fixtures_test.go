package state

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestApplyFixtureUnknown(t *testing.T) {
	e, _ := newTestEngine(t)
	err := e.ApplyFixture(context.Background(), "SIM-1", "nope", nil)
	if !errors.Is(err, ErrBadFixture) {
		t.Fatalf("want ErrBadFixture, got %v", err)
	}
}

func TestStatusBarOverride(t *testing.T) {
	e, run := newTestEngine(t)
	err := e.ApplyFixture(context.Background(), "SIM-1", "statusbar", map[string]any{
		"time":         "9:41",
		"batteryLevel": float64(100),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"status_bar", "SIM-1", "override", "--batteryLevel", "100", "--time", "9:41"}
	if !reflect.DeepEqual(run.lastCall(), want) {
		t.Fatalf("got %v, want %v", run.lastCall(), want)
	}
}

func TestStatusBarClear(t *testing.T) {
	e, run := newTestEngine(t)
	if err := e.ApplyFixture(context.Background(), "SIM-1", "statusbar", map[string]any{"clear": true}); err != nil {
		t.Fatal(err)
	}
	want := []string{"status_bar", "SIM-1", "clear"}
	if !reflect.DeepEqual(run.lastCall(), want) {
		t.Fatalf("got %v, want %v", run.lastCall(), want)
	}
}

func TestStatusBarClearFalseIsNotAFlag(t *testing.T) {
	e, run := newTestEngine(t)
	err := e.ApplyFixture(context.Background(), "SIM-1", "statusbar", map[string]any{
		"clear": false,
		"time":  "9:41",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"status_bar", "SIM-1", "override", "--time", "9:41"}
	if !reflect.DeepEqual(run.lastCall(), want) {
		t.Fatalf("got %v, want %v", run.lastCall(), want)
	}
}

func TestStatusBarRejectsUnknownKey(t *testing.T) {
	e, _ := newTestEngine(t)
	err := e.ApplyFixture(context.Background(), "SIM-1", "statusbar", map[string]any{
		"-": "x",
	})
	if !errors.Is(err, ErrBadFixture) {
		t.Fatalf("want ErrBadFixture, got %v", err)
	}
}

func TestFixtureValuesRejectFlagInjection(t *testing.T) {
	e, _ := newTestEngine(t)
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"statusbar", map[string]any{"time": "--wifiBars"}},
		{"locale", map[string]any{"language": []any{"-x"}}},
		{"locale", map[string]any{"locale": "-x"}},
		{"privacy", map[string]any{"action": "grant", "service": "photos", "bundle_id": "-x"}},
		{"url", map[string]any{"url": "--foo"}},
		{"timezone", map[string]any{"tz": "-x"}},
	}
	for _, c := range cases {
		if err := e.ApplyFixture(context.Background(), "SIM-1", c.name, c.payload); !errors.Is(err, ErrBadFixture) {
			t.Fatalf("%s %v: want ErrBadFixture, got %v", c.name, c.payload, err)
		}
	}
}

func TestStatusBarRejectsWrongTypedValues(t *testing.T) {
	e, _ := newTestEngine(t)
	cases := []map[string]any{
		{"time": float64(123)},        // string flag with a number
		{"batteryLevel": "full"},      // number flag with a string
		{"wifiBars": true},            // number flag with a bool
		{"operatorName": float64(42)}, // string flag with a number
	}
	for _, payload := range cases {
		if err := e.ApplyFixture(context.Background(), "SIM-1", "statusbar", payload); !errors.Is(err, ErrBadFixture) {
			t.Fatalf("%v: want ErrBadFixture, got %v", payload, err)
		}
	}
}

func TestStatusBarEmptyPayload(t *testing.T) {
	e, _ := newTestEngine(t)
	if err := e.ApplyFixture(context.Background(), "SIM-1", "statusbar", map[string]any{}); !errors.Is(err, ErrBadFixture) {
		t.Fatalf("want ErrBadFixture, got %v", err)
	}
}

func TestPrivacyGrant(t *testing.T) {
	e, run := newTestEngine(t)
	err := e.ApplyFixture(context.Background(), "SIM-1", "privacy", map[string]any{
		"action": "grant", "service": "photos", "bundle_id": "com.example.app",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"privacy", "SIM-1", "grant", "photos", "com.example.app"}
	if !reflect.DeepEqual(run.lastCall(), want) {
		t.Fatalf("got %v, want %v", run.lastCall(), want)
	}
}

func TestPrivacyGrantRequiresBundle(t *testing.T) {
	e, _ := newTestEngine(t)
	err := e.ApplyFixture(context.Background(), "SIM-1", "privacy", map[string]any{
		"action": "grant", "service": "photos",
	})
	if !errors.Is(err, ErrBadFixture) {
		t.Fatalf("want ErrBadFixture, got %v", err)
	}
}

func TestPrivacyResetWithoutBundle(t *testing.T) {
	e, run := newTestEngine(t)
	err := e.ApplyFixture(context.Background(), "SIM-1", "privacy", map[string]any{
		"action": "reset", "service": "all",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"privacy", "SIM-1", "reset", "all"}
	if !reflect.DeepEqual(run.lastCall(), want) {
		t.Fatalf("got %v, want %v", run.lastCall(), want)
	}
}

func TestPushFixture(t *testing.T) {
	e, run := newTestEngine(t)
	err := e.ApplyFixture(context.Background(), "SIM-1", "push", map[string]any{
		"bundle_id": "com.example.app",
		"payload":   map[string]any{"aps": map[string]any{"alert": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"push", "SIM-1", "com.example.app", "-"}
	if !reflect.DeepEqual(run.lastCall(), want) {
		t.Fatalf("got %v, want %v", run.lastCall(), want)
	}
	if len(run.stdins) != 1 || !strings.Contains(string(run.stdins[0]), `"aps"`) {
		t.Fatalf("bad stdin: %v", run.stdins)
	}
}

func TestPushFixtureRequiresAps(t *testing.T) {
	e, _ := newTestEngine(t)
	err := e.ApplyFixture(context.Background(), "SIM-1", "push", map[string]any{
		"bundle_id": "com.example.app",
		"payload":   map[string]any{"alert": "hi"},
	})
	if !errors.Is(err, ErrBadFixture) {
		t.Fatalf("want ErrBadFixture, got %v", err)
	}
}

func TestLocaleFixture(t *testing.T) {
	e, run := newTestEngine(t)
	err := e.ApplyFixture(context.Background(), "SIM-1", "locale", map[string]any{
		"language": []any{"zh-Hans", "en"},
		"locale":   "zh_CN",
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := run.callStrings()
	if len(calls) != 2 ||
		calls[0] != "spawn SIM-1 defaults write .GlobalPreferences AppleLanguages -array zh-Hans en" ||
		calls[1] != "spawn SIM-1 defaults write .GlobalPreferences AppleLocale -string zh_CN" {
		t.Fatalf("got %v", calls)
	}
}

func TestLocaleFixtureEmpty(t *testing.T) {
	e, _ := newTestEngine(t)
	if err := e.ApplyFixture(context.Background(), "SIM-1", "locale", map[string]any{}); !errors.Is(err, ErrBadFixture) {
		t.Fatalf("want ErrBadFixture, got %v", err)
	}
}

func TestTimezoneFixture(t *testing.T) {
	e, run := newTestEngine(t)
	if err := e.ApplyFixture(context.Background(), "SIM-1", "timezone", map[string]any{"tz": "America/Los_Angeles"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"spawn", "SIM-1", "launchctl", "setenv", "TZ", "America/Los_Angeles"}
	if !reflect.DeepEqual(run.lastCall(), want) {
		t.Fatalf("got %v, want %v", run.lastCall(), want)
	}
}

func TestURLFixture(t *testing.T) {
	e, run := newTestEngine(t)
	if err := e.ApplyFixture(context.Background(), "SIM-1", "url", map[string]any{"url": "myapp://deep/link"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"openurl", "SIM-1", "myapp://deep/link"}
	if !reflect.DeepEqual(run.lastCall(), want) {
		t.Fatalf("got %v, want %v", run.lastCall(), want)
	}
}
