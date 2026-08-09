package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/BariBariGood/manzanas/internal/client"
	"github.com/BariBariGood/manzanas/proto"
)

// Composite element tools: the daemon resolves an element matcher and
// acts on the match in one request, so an agent never has to compute tap
// coordinates from a tree itself. Matcher semantics are shared with
// wait_for_element (see tools_wait.go).

// matcherProps returns the schema properties shared by every tool that
// selects an element by matcher, plus the shared timing knobs.
func matcherProps() map[string]map[string]any {
	return map[string]map[string]any{
		"lease_id": {"type": "string", "description": "Active lease ID from lease_acquire."},
		"label": {"type": "string",
			"description": "Match by accessibility label (visible text), substring by default. The most common matcher, e.g. \"Continue\"."},
		"role": {"type": "string",
			"description": "Match by element role, exact: Button, Cell, TextField, StaticText, Link, Image, ..."},
		"value": {"type": "string",
			"description": "Match by the element's current value (e.g. a text field's content), substring by default."},
		"id": {"type": "string",
			"description": "Match by accessibility identifier (testID), exact. Most reliable when the app sets one."},
		"placeholder": {"type": "string",
			"description": "Match by placeholder text (also matches an empty field's value), substring by default. Best way to find empty text fields."},
		"exact": {"type": "boolean", "default": false,
			"description": "Require label/value/placeholder to match exactly instead of by substring."},
		"predicate": {"type": "object",
			"description": "Structured predicate — a strict alternative to the flat matcher fields above (do not combine with label/role/value/id/placeholder). Fields: text (exact visible text), text_contains (substring), text_regex (RE2 regex) — at most one text form; type (element role, e.g. Button, Cell); accessibility_id (testID, exact); bounds_hint (top_half | bottom_half | left_half | right_half | center — where on screen the element sits); near ({predicate, direction: left|right|above|below, max_distance?} — the element lies in that direction from another uniquely-matching element, e.g. the TextField right of the \"Email\" label); parent_of (a predicate on a descendant; resolves the enclosing element, e.g. the Cell containing text \"Alice\"); index (0-based pick when several elements legitimately match). Unlike the flat matcher there is NO best-match ranking: several matches without index fail with ambiguous_match listing every candidate — tighten the predicate or add index. Example: {\"type\":\"Button\",\"text\":\"Delete\",\"near\":{\"predicate\":{\"text\":\"Alice\"},\"direction\":\"right\"}}."},
		"timeout_ms": {"type": "integer", "default": 10000,
			"description": "How long to keep polling for a match before failing, in milliseconds (max 120000)."},
		"interval_ms": {"type": "integer", "default": 500,
			"description": "Delay between polls in milliseconds (min 10)."},
	}
}

// matcherKeys are the argument names copied verbatim into the action
// payload (arg names match the daemon payload fields 1:1).
var matcherKeys = []string{"label", "role", "value", "id", "placeholder",
	"exact", "predicate", "timeout_ms", "interval_ms"}

// elementPayload copies the matcher arguments plus any extra keys into a
// daemon action payload.
func elementPayload(args map[string]any, extraKeys ...string) map[string]any {
	p := map[string]any{}
	for _, k := range append(append([]string{}, matcherKeys...), extraKeys...) {
		if v, ok := args[k]; ok {
			p[k] = v
		}
	}
	return p
}

// dispatchElement runs one element action and shapes the result:
// ui_changed is derived from the before/after tree hashes when present,
// and matcher misses come back with a next-step hint.
func dispatchElement(ctx context.Context, s *Server, kind, leaseID string, payload map[string]any) ([]map[string]any, error) {
	// Always ask for the before/after tree hashes so the result carries a
	// post-action change signal and agents can skip a follow-up screenshot.
	payload["ax_hashes"] = true
	res, err := s.client.Dispatch(ctx, proto.ActionRequest{
		LeaseID: leaseID, Kind: kind, Payload: payload})
	if err != nil {
		return nil, matcherHint(kind, err)
	}
	if err := actionErr(res); err != nil {
		return nil, matcherHint(kind, err)
	}
	// A scroll that never swiped (element already visible) did not change
	// the UI; skip the change signal instead of reporting hash jitter.
	if kind != "scroll_to_element" || numField(res.Result, "scrolls") != 0 {
		if res.Result == nil {
			res.Result = map[string]any{}
		}
		before, bok := res.Result["ax_before"].(string)
		after, aok := res.Result["ax_after"].(string)
		if bok && aok {
			res.Result["ui_changed"] = before != after
		} else {
			// The daemon hashes the tree best-effort (a slow or warming
			// a11y bridge degrades to a missing hash); report "unknown"
			// explicitly instead of silently dropping the promised field.
			res.Result["ui_changed"] = nil
		}
	}
	return jsonContent(res.Result)
}

// numField reads a numeric result field regardless of whether it came
// from the daemon in-process (int) or through JSON decoding (float64).
func numField(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return -1
}

// matcherHint appends the next step to matcher-miss errors so an agent
// that has never read the docs knows what to do. The hint is kind-aware:
// a scroll_to_element failure must not recommend scroll_to_element again,
// and an absent-wait timeout means the element is still there.
func matcherHint(kind string, err error) error {
	var ae *client.APIError
	if !errors.As(err, &ae) {
		return err
	}
	switch ae.Code {
	case "timeout":
		if kind == "wait_for_element_absent" {
			return fmt.Errorf("%s; the element is still on screen — call ui_tree to see what is keeping it there (e.g. a sheet, spinner, or alert) and dismiss it before retrying", ae.Message)
		}
		if kind == "scroll_to_element" {
			return fmt.Errorf("%s; call ui_tree to see what is on screen now — the element is probably on a different screen", ae.Message)
		}
		return fmt.Errorf("%s; call ui_tree to see what is on screen now and adjust the matcher (or use scroll_to_element if the element is further down the page)", ae.Message)
	case "ambiguous_match":
		return fmt.Errorf("%s; add index to the predicate or tighten it (type, accessibility_id, bounds_hint, near) so exactly one candidate matches", ae.Message)
	case "off_viewport":
		if kind == "scroll_to_element" {
			return fmt.Errorf("%s; call ui_tree to inspect the layout — do not retry the same scroll", ae.Message)
		}
		return fmt.Errorf("%s; call scroll_to_element with the same matcher to bring it into view first", ae.Message)
	}
	return err
}

func toolTapElement() Tool {
	return Tool{
		Name:        "tap_element",
		Description: "Find an element by matcher (label/id/role/value/placeholder) and tap it, in one call. Polls until the element appears, so use it right after navigation without a separate wait. Prefer this over observe+tap coordinates. Fails with a hint if nothing matches within timeout_ms.",
		InputSchema: schema(mergeProps(mergeProps(matcherProps(), logProps()), map[string]map[string]any{
			"anchor": {"type": "string", "enum": []string{"start", "center", "end"}, "default": "center",
				"description": "Where on the element's frame to tap: center (default), or a point near the leading (start) / trailing (end) edge — useful for an inline link at the end of a sentence."},
		}), "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			return dispatchElement(ctx, s, "tap_element", leaseID,
				elementPayload(args, "anchor", "capture_logs", "log_process"))
		},
	}
}

func toolTypeIntoElement() Tool {
	return Tool{
		Name:        "type_into_element",
		Description: "Find an element by matcher, tap it to focus it, then type text into it — all in one call. Use placeholder to match empty text fields. strategy \"paste\" delivers the text in one shot (best for long or special-character text).",
		InputSchema: schema(mergeProps(mergeProps(matcherProps(), logProps()), map[string]map[string]any{
			"text": {"type": "string", "description": "The text to type into the matched element."},
			"anchor": {"type": "string", "enum": []string{"start", "center", "end"}, "default": "center",
				"description": "Where on the element's frame to tap when focusing it: center (default), or a point near the leading (start) / trailing (end) edge."},
			"strategy": {"type": "string", "enum": []string{"hid", "paste"}, "default": "hid",
				"description": "hid types one keystroke per character; paste puts the text on the pasteboard and sends Cmd-V (simulators only, overwrites the pasteboard, field must accept paste)."},
			"require_focus": {"type": "boolean", "default": false,
				"description": "Verify the on-screen keyboard is visible before typing, so keystrokes cannot land outside a field. Leave false if the simulator has Connect Hardware Keyboard active (the software keyboard never shows)."},
		}), "lease_id", "text"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			if _, err := reqStr(args, "text"); err != nil {
				return nil, err
			}
			return dispatchElement(ctx, s, "type_into_element", leaseID,
				elementPayload(args, "text", "strategy", "require_focus", "anchor",
					"capture_logs", "log_process"))
		},
	}
}

func toolScrollToElement() Tool {
	return Tool{
		Name:        "scroll_to_element",
		Description: "Scroll until an element matching the matcher is visible on screen, with bounded swipe attempts. Use it when tap_element reports the element is outside the viewport, or when the target is further down a list/page. Errors say whether the element was never in the tree (wrong screen) or matched but could not be brought into view.",
		InputSchema: schema(mergeProps(mergeProps(matcherProps(), logProps()), map[string]map[string]any{
			"direction": {"type": "string", "enum": []string{"down", "up", "left", "right"}, "default": "down",
				"description": "Which content edge to reveal while the element is not on screen yet: down scrolls toward content below the fold (default). Once the element is in the tree, swipes automatically move toward it."},
			"max_scrolls": {"type": "integer", "default": 8,
				"description": "Maximum swipe attempts before giving up (max 30)."},
			"timeout_ms": {"type": "integer", "default": 30000,
				"description": "Overall budget in milliseconds (max 120000)."},
		}), "lease_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			leaseID, err := requireLease(args)
			if err != nil {
				return nil, err
			}
			return dispatchElement(ctx, s, "scroll_to_element", leaseID,
				elementPayload(args, "direction", "max_scrolls", "capture_logs", "log_process"))
		},
	}
}

// logProps are the opt-in log-capture schema properties shared by the
// actions that support capture_logs.
func logProps() map[string]map[string]any {
	return map[string]map[string]any{
		"capture_logs": {"type": "boolean", "default": false,
			"description": "Collect the simulator's os_log lines emitted during the action window and return them as result.logs (also journaled as an artifact next to this step). Simulators only. Use log_process to cut noise."},
		"log_process": {"type": "string",
			"description": "Only capture log lines from this process name (the app's executable name, e.g. \"MobileSafari\"). Setting it implies capture_logs."},
	}
}

// mergeProps overlays extra schema properties onto base (extra wins).
func mergeProps(base, extra map[string]map[string]any) map[string]map[string]any {
	for k, v := range extra {
		base[k] = v
	}
	return base
}
