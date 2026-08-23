package actions

import (
	"context"
	"testing"
)

func collectTexts(t *testing.T, res map[string]any) [][]string {
	t.Helper()
	screens, ok := res["screens_texts"].([]map[string]any)
	if !ok {
		t.Fatalf("screens_texts = %#v", res["screens_texts"])
	}
	out := make([][]string, 0, len(screens))
	for _, s := range screens {
		boxes, ok := s["texts"].([]map[string]any)
		if !ok {
			t.Fatalf("texts = %#v", s["texts"])
		}
		texts := make([]string, 0, len(boxes))
		for _, b := range boxes {
			texts = append(texts, b["text"].(string))
		}
		out = append(out, texts)
	}
	return out
}

func TestScrollCollectHarvestsAndDedups(t *testing.T) {
	// "Header" is sticky across screens: it must be harvested once, on
	// the first screen it was seen on.
	d := &fakeListDriver{seq: []observation{
		ocrScreen("Header", "Alpha", "Bravo"),
		ocrScreen("Header", "Charlie", "Delta"),
		ocrScreen("Header", "Charlie", "Delta"),
	}}
	res, err := elemScrollCollect(context.Background(), d, "UDID",
		fastUntil(map[string]any{"max_scrolls": 10.0}))
	if err != nil {
		t.Fatalf("scroll_collect: %v", err)
	}
	if res["end_of_list"] != true {
		t.Fatalf("res = %v", res)
	}
	screens := collectTexts(t, res)
	if len(screens) < 2 {
		t.Fatalf("screens = %v", screens)
	}
	if len(screens[0]) != 3 || screens[0][0] != "Header" {
		t.Fatalf("first screen = %v", screens[0])
	}
	// Second screen: Header deduped away, only the new rows remain.
	if len(screens[1]) != 2 || screens[1][0] != "Charlie" || screens[1][1] != "Delta" {
		t.Fatalf("second screen = %v", screens[1])
	}
	if res["collected"] != 5 {
		t.Fatalf("collected = %v, want 5", res["collected"])
	}
}

func TestScrollCollectHarvestsSingleCharRuns(t *testing.T) {
	d := &fakeListDriver{seq: []observation{
		ocrScreen("1", "X", "Alpha"),
		ocrScreen("1", "X", "Alpha"),
	}}
	res, err := elemScrollCollect(context.Background(), d, "UDID",
		fastUntil(map[string]any{}))
	if err != nil {
		t.Fatalf("scroll_collect: %v", err)
	}
	screens := collectTexts(t, res)
	if len(screens[0]) != 3 || screens[0][0] != "1" || screens[0][1] != "X" {
		t.Fatalf("first screen = %v, want single-char runs harvested", screens[0])
	}
}

func TestScrollCollectRectFiltersBoxes(t *testing.T) {
	d := &fakeListDriver{seq: []observation{
		// Rows at y=150, 270, 390, ... (ocrScreen layout).
		ocrScreen("Inside", "Also Inside", "Outside Below"),
		ocrScreen("Inside", "Also Inside", "Outside Below"),
	}}
	res, err := elemScrollCollect(context.Background(), d, "UDID",
		fastUntil(map[string]any{
			"rect": map[string]any{"x": 0.0, "y": 100.0, "w": 600.0, "h": 250.0},
		}))
	if err != nil {
		t.Fatalf("scroll_collect: %v", err)
	}
	screens := collectTexts(t, res)
	if len(screens[0]) != 2 || screens[0][0] != "Inside" || screens[0][1] != "Also Inside" {
		t.Fatalf("first screen = %v, want the two boxes inside the rect", screens[0])
	}
}

func TestScrollCollectRejectsBadRect(t *testing.T) {
	d := &fakeListDriver{seq: []observation{ocrScreen("Alpha")}}
	for _, r := range []any{
		"everything",
		map[string]any{"x": 0.0, "y": 0.0},
		map[string]any{"x": 0.0, "y": 0.0, "w": -1.0, "h": 10.0},
		map[string]any{"x": 0.0, "y": 0.0, "w": 10.0, "h": 0.0},
	} {
		_, err := elemScrollCollect(context.Background(), d, "UDID",
			fastUntil(map[string]any{"rect": r}))
		if e, ok := err.(*Error); !ok || e.Code != "bad_request" {
			t.Fatalf("rect=%v: err = %v, want bad_request", r, err)
		}
	}
}

func TestMirrorScrollCollectDeclaresFidelity(t *testing.T) {
	f, b, _ := testMirrorBackend(t)
	f.boxes = []map[string]any{
		{"text": "Row One", "confidence": 0.95, "x": 40, "y": 300, "w": 200, "h": 30},
		{"text": "Row Two", "confidence": 0.92, "x": 40, "y": 380, "w": 200, "h": 30},
	}
	res, err := deviceDispatch(t, b, "scroll_collect", map[string]any{
		"max_scrolls": 2, "timeout_ms": 2000, "interval_ms": 10})
	if err != nil {
		t.Fatalf("scroll_collect: %v", err)
	}
	if res.Result["backend"] != "mirror" || res.Result["fidelity"] != "ocr" {
		t.Fatalf("res = %+v", res.Result)
	}
	if res.Result["end_of_list"] != true {
		t.Fatalf("static OCR boxes should read as end-of-list: %+v", res.Result)
	}
	if res.Result["collected"] != 2 {
		t.Fatalf("collected = %v, want 2", res.Result["collected"])
	}
}
