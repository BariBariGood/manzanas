package actions

import (
	"context"
	"errors"
	"testing"
)

func TestPasteboardSet(t *testing.T) {
	f := newFakeRunner()
	b := testBackend(f)
	res, err := handlePasteboardSet(context.Background(), b, "UDID", map[string]any{"text": "héllo"})
	if err != nil {
		t.Fatalf("pasteboard_set: %v", err)
	}
	if res["copied_runes"] != 5 {
		t.Fatalf("copied_runes = %v, want 5", res["copied_runes"])
	}
	want := "xcrun simctl pbcopy UDID <stdin:héllo>"
	if got := f.argvs(); len(got) != 1 || got[0] != want {
		t.Fatalf("argv = %v, want [%s]", got, want)
	}
}

func TestPasteboardSetAllowsEmpty(t *testing.T) {
	f := newFakeRunner()
	b := testBackend(f)
	res, err := handlePasteboardSet(context.Background(), b, "UDID", map[string]any{"text": ""})
	if err != nil {
		t.Fatalf("pasteboard_set empty: %v", err)
	}
	if res["copied_runes"] != 0 {
		t.Fatalf("copied_runes = %v, want 0", res["copied_runes"])
	}
}

func TestPasteboardSetRequiresText(t *testing.T) {
	f := newFakeRunner()
	b := testBackend(f)
	_, err := handlePasteboardSet(context.Background(), b, "UDID", map[string]any{})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "bad_request" {
		t.Fatalf("want bad_request, got %v", err)
	}
}

func TestPasteboardGet(t *testing.T) {
	f := newFakeRunner()
	f.stdout["pbpaste"] = "clipboard content"
	b := testBackend(f)
	res, err := handlePasteboardGet(context.Background(), b, "UDID", nil)
	if err != nil {
		t.Fatalf("pasteboard_get: %v", err)
	}
	if res["text"] != "clipboard content" {
		t.Fatalf("text = %q", res["text"])
	}
	want := "xcrun simctl pbpaste UDID"
	if got := f.argvs(); len(got) != 1 || got[0] != want {
		t.Fatalf("argv = %v, want [%s]", got, want)
	}
}

func TestPasteboardGetError(t *testing.T) {
	f := newFakeRunner()
	f.errs["pbpaste"] = "boom"
	b := testBackend(f)
	_, err := handlePasteboardGet(context.Background(), b, "UDID", nil)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "internal" {
		t.Fatalf("want internal, got %v", err)
	}
}

// stdinlessRunner implements only Runner, not InputRunner.
type stdinlessRunner struct{}

func (stdinlessRunner) Run(context.Context, string, ...string) ([]byte, []byte, error) {
	return nil, nil, nil
}

func TestPasteboardSetWithoutInputRunner(t *testing.T) {
	b := NewAXe(WithRunner(stdinlessRunner{}), WithAXePath(""))
	_, err := handlePasteboardSet(context.Background(), b, "UDID", map[string]any{"text": "x"})
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "unavailable" {
		t.Fatalf("want unavailable, got %v", err)
	}
}
