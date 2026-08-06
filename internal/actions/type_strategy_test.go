package actions

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// keyboardTreeJSON is a describe-ui document whose window shows a text
// field plus on-screen keyboard keys — the focus guard's evidence.
func keyboardTreeJSON(labels ...string) string {
	cells := make([]string, 0, len(labels)+1)
	for i, l := range labels {
		cells = append(cells, fmt.Sprintf(
			`{"role":"Cell","AXLabel":%q,"frame":{"x":0,"y":%d,"width":400,"height":44}}`, l, 100+i*44))
	}
	cells = append(cells, `{"role":"Key","AXLabel":"a","frame":{"x":0,"y":700,"width":30,"height":40}}`)
	return `[{"role":"Window","children":[` + strings.Join(cells, ",") + `]}]`
}

func shortFocusWait(t *testing.T) {
	t.Helper()
	oldT, oldI := focusWaitTimeout, focusWaitInterval
	focusWaitTimeout, focusWaitInterval = 200*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { focusWaitTimeout, focusWaitInterval = oldT, oldI })
}

func TestTypePasteStrategyCopiesThenChords(t *testing.T) {
	f := newFakeRunner()
	b := testBackend(f)
	res, err := handleType(context.Background(), b, udid,
		map[string]any{"text": "héllo", "strategy": "paste"})
	if err != nil {
		t.Fatalf("type paste: %v", err)
	}
	if res["typed_runes"] != 5 || res["strategy"] != "paste" {
		t.Fatalf("result = %v, want typed_runes=5 strategy=paste", res)
	}
	argvs := f.argvs()
	pb, combo := -1, -1
	for i, a := range argvs {
		if strings.Contains(a, "pbcopy TEST-UDID <stdin:héllo>") {
			pb = i
		}
		if strings.Contains(a, "key-combo --modifiers 227 --key 25 --udid TEST-UDID") {
			combo = i
		}
	}
	if pb < 0 || combo < 0 || pb > combo {
		t.Fatalf("want pbcopy before key-combo, calls: %v", argvs)
	}
	if typeCall := lastArgvContaining(argvs, "/fake/axe type "); typeCall != "" {
		t.Fatalf("paste strategy must not run per-keystroke type: %s", typeCall)
	}
}

func TestTypePasteStrategyOldAXeIsUnavailable(t *testing.T) {
	f := newFakeRunner()
	f.errs["key-combo"] = "Error: Unexpected argument 'key-combo'. Help for axe:"
	b := testBackend(f)
	_, err := handleType(context.Background(), b, udid,
		map[string]any{"text": "hello", "strategy": "paste"})
	if e, ok := err.(*Error); !ok || e.Code != "unavailable" {
		t.Fatalf("err = %v, want unavailable", err)
	}
}

func TestTypeRejectsUnknownStrategy(t *testing.T) {
	b := testBackend(newFakeRunner())
	_, err := handleType(context.Background(), b, udid,
		map[string]any{"text": "hello", "strategy": "dictate"})
	if e, ok := err.(*Error); !ok || e.Code != "bad_request" {
		t.Fatalf("err = %v, want bad_request", err)
	}
}

func TestTypeRequireFocusRejectsWithoutKeyboard(t *testing.T) {
	shortFocusWait(t)
	f := newFakeRunner()
	f.stdout["describe-ui"] = treeJSON("Settings")
	b := testBackend(f)
	_, err := handleType(context.Background(), b, udid,
		map[string]any{"text": "hello", "require_focus": true})
	if e, ok := err.(*Error); !ok || e.Code != "focus_required" {
		t.Fatalf("err = %v, want focus_required", err)
	}
	if c := lastArgvContaining(f.argvs(), "/fake/axe type "); c != "" {
		t.Fatalf("focus guard must block typing, got: %s", c)
	}
}

func TestTypeRequireFocusTypesWithKeyboard(t *testing.T) {
	shortFocusWait(t)
	f := newFakeRunner()
	f.stdout["describe-ui"] = keyboardTreeJSON("Search")
	b := testBackend(f)
	res, err := handleType(context.Background(), b, udid,
		map[string]any{"text": "hello", "require_focus": true})
	if err != nil {
		t.Fatalf("type: %v", err)
	}
	if res["typed_runes"] != 5 {
		t.Fatalf("typed_runes = %v, want 5", res["typed_runes"])
	}
}

func TestTypeIntoElementRequireFocusRejectsWithoutKeyboard(t *testing.T) {
	shortFocusWait(t)
	b, r := compositeBackend(seqStep{stdout: treeJSON("Search")})
	_, err := handleTypeIntoElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "Search", "text": "hello", "require_focus": true}))
	if e, ok := err.(*Error); !ok || e.Code != "focus_required" {
		t.Fatalf("err = %v, want focus_required", err)
	}
	if c := lastCall(r, "type"); c != nil {
		t.Fatalf("focus guard must block typing, got: %v", c)
	}
	if c := lastCall(r, "tap"); c == nil {
		t.Fatal("the focusing tap should still have run")
	}
}

func TestTypeIntoElementRequireFocusTypesWithKeyboard(t *testing.T) {
	shortFocusWait(t)
	b, r := compositeBackend(
		seqStep{stdout: treeJSON("Search")},
		seqStep{stdout: keyboardTreeJSON("Search")},
	)
	res, err := handleTypeIntoElement(context.Background(), b, "UDID",
		fastWait(map[string]any{"label": "Search", "text": "hello", "require_focus": true}))
	if err != nil {
		t.Fatalf("type_into_element: %v", err)
	}
	if res["typed_runes"] != 5 {
		t.Fatalf("typed_runes = %v, want 5", res["typed_runes"])
	}
	if c := lastCall(r, "type"); c == nil {
		t.Fatal("no type invocation recorded")
	}
}

func TestTypeIntoElementPasteStrategy(t *testing.T) {
	f := newFakeRunner()
	f.stdout["describe-ui"] = treeJSON("Search")
	b := testBackend(f)
	res, err := handleTypeIntoElement(context.Background(), b, udid,
		fastWait(map[string]any{"label": "Search", "text": "hello", "strategy": "paste"}))
	if err != nil {
		t.Fatalf("type_into_element paste: %v", err)
	}
	if res["typed_runes"] != 5 || res["strategy"] != "paste" {
		t.Fatalf("result = %v, want typed_runes=5 strategy=paste", res)
	}
	argvs := f.argvs()
	if lastArgvContaining(argvs, "pbcopy") == "" {
		t.Fatalf("no pbcopy call, calls: %v", argvs)
	}
	if lastArgvContaining(argvs, "key-combo") == "" {
		t.Fatalf("no key-combo call, calls: %v", argvs)
	}
}

func TestTypePasteWarmKeyCombo(t *testing.T) {
	f := newFakeRunner()
	b := testBackend(f)
	var warm [][]any
	b.SetWarmHID(func(_ context.Context, _, op string, args map[string]any) error {
		warm = append(warm, []any{op, args["modifiers"], args["keycode"]})
		return nil
	})
	if _, err := handleType(context.Background(), b, udid,
		map[string]any{"text": "hello", "strategy": "paste"}); err != nil {
		t.Fatalf("type paste: %v", err)
	}
	if len(warm) != 1 || warm[0][0] != "key_combo" {
		t.Fatalf("warm HID calls = %v, want one key_combo", warm)
	}
	if c := lastArgvContaining(f.argvs(), "key-combo"); c != "" {
		t.Fatalf("cold key-combo CLI invoked despite warm helper: %s", c)
	}
}

func TestDeviceTypeIntoRejectsPaste(t *testing.T) {
	b := &DeviceBackend{}
	_, err := b.typeInto(context.Background(), udid, "hello", typeOpts{strategy: typeStrategyPaste})
	if e, ok := err.(*Error); !ok || e.Code != "not_implemented" {
		t.Fatalf("err = %v, want not_implemented", err)
	}
}

func lastArgvContaining(argvs []string, sub string) string {
	for i := len(argvs) - 1; i >= 0; i-- {
		if strings.Contains(argvs[i], sub) {
			return argvs[i]
		}
	}
	return ""
}
