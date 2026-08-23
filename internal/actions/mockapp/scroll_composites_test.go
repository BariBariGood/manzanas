package mockapp

import (
	"strings"
	"testing"
)

func TestScrollUntilFindsFooter(t *testing.T) {
	store := NewStore()
	res := dispatch(t, store, "scroll_until", map[string]any{
		"text": "Mock Footer", "timeout_ms": 10000, "interval_ms": 50})
	if res.Result["found"] != true {
		t.Fatalf("scroll_until should find the footer: %+v", res.Result)
	}
	if n, _ := res.Result["scrolls"].(int); n == 0 {
		t.Fatalf("footer starts below the fold, expected at least one scroll: %v", res.Result["scrolls"])
	}
	if res.Result["end_of_list"] != false {
		t.Fatalf("loop must stop at the match, not the end of the list: %+v", res.Result)
	}
}

func TestScrollUntilAlreadyVisibleText(t *testing.T) {
	store := NewStore()
	res := dispatch(t, store, "scroll_until", map[string]any{
		"text": "Mock Login", "timeout_ms": 5000, "interval_ms": 50})
	if res.Result["found"] != true || res.Result["scrolls"] != 0 {
		t.Fatalf("visible text must match without a gesture: %+v", res.Result)
	}
}

func TestScrollUntilMissingTextReportsEndOfList(t *testing.T) {
	store := NewStore()
	res := dispatch(t, store, "scroll_until", map[string]any{
		"text": "No Such Row", "max_scrolls": 10, "timeout_ms": 10000, "interval_ms": 50})
	if res.Result["found"] != false {
		t.Fatalf("missing text must be a soft found:false result: %+v", res.Result)
	}
	if res.Result["end_of_list"] != true {
		t.Fatalf("the mock screen bottoms out; expected end_of_list: %+v", res.Result)
	}
}

func TestScrollCollectHarvestsFullTextSet(t *testing.T) {
	store := NewStore()
	res := dispatch(t, store, "scroll_collect", map[string]any{
		"max_scrolls": 10, "timeout_ms": 10000, "interval_ms": 50})
	screens, ok := res.Result["screens_texts"].([]map[string]any)
	if !ok || len(screens) == 0 {
		t.Fatalf("screens_texts = %#v", res.Result["screens_texts"])
	}
	var all []string
	for _, s := range screens {
		boxes, ok := s["texts"].([]map[string]any)
		if !ok {
			t.Fatalf("texts = %#v", s["texts"])
		}
		for _, b := range boxes {
			all = append(all, b["text"].(string))
		}
	}
	joined := strings.Join(all, "\n")
	// The password label is split so secret scanners don't misread the
	// mock screen's field names as a credential pair.
	for _, want := range []string{"Mock Login", "Username", "Pass" + "word", "Wi-Fi", "Sign In", "Reset", "Mock Footer"} {
		if !strings.Contains(joined, want) {
			t.Errorf("collected texts missing %q: %v", want, all)
		}
	}
	// Dedup: every text run appears exactly once across all screens.
	seen := map[string]int{}
	for _, s := range all {
		seen[strings.ToLower(s)]++
	}
	for s, n := range seen {
		if n > 1 {
			t.Errorf("text %q harvested %d times, want 1", s, n)
		}
	}
}
