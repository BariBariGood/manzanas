---
name: ui-audit-and-predicates
description: QA a screen with the deterministic audit action and target elements with structured predicates instead of hand-parsing describe-ui trees or eyeballing screenshots. Use for fleet QA sessions checking touch targets, clipping, alignment, spacing, safe-area violations, and missing accessibility labels.
---

# UI audit + predicates: stop hand-parsing trees

Two tools replace manual tree/screenshot inspection during QA:

1. **`audit`** — deterministic geometry checks over the accessibility tree,
   returning findings (measured evidence, never pass/fail verdicts) plus an
   annotated screenshot with a labeled red box per finding. Both are
   journaled automatically.
2. **Predicates** — strict element matching for `tap_element`,
   `type_into_element`, `wait_for_element`, `scroll_to_element`. A predicate
   must resolve to exactly one element; ambiguity fails loudly with every
   candidate listed instead of silently guessing.

## Audit a screen

```sh
manzanas audit --lease $LID -o annotated.png            # all six checks
manzanas audit --lease $LID --checks touch_target,missing_labels
manzanas audit --lease $LID --label "Sign up"           # scope to one subtree
manzanas audit --lease $LID --region 0,0,393,400        # scope to a screen area
```

MCP agents call the `audit` tool with the same fields (`checks`, `region`,
matcher fields, `min_touch_pt`, `alignment_tolerance_pt`,
`spacing_tolerance_pt`, `safe_area_insets`).

Checks: `touch_target` (< 44x44pt interactive), `clipping` (past screen or
non-scrolling parent bounds), `alignment` (near-miss edge deltas ≤ 4pt),
`spacing` (sibling gaps deviating > 4pt from the median), `safe_area`
(interactive elements in the insets), `missing_labels` (interactive
elements a screen reader cannot name).

Reading results:
- Each finding has `check`, `ref` (F1, F2, ... — matches the red box on the
  annotated screenshot), the element's role/label/id/frame, `measured`
  values, and an `evidence` sentence.
- Findings are EVIDENCE, not verdicts. You decide what matters: a 30x30
  close button is probably a real defect; a deliberately compact stepper
  may not be.
- Dense repeated grids (keyboards, emoji pickers, calendar cells) are
  auto-suppressed; `suppressed_elements` reports how many.
- System chrome is auto-suppressed too: scroll-indicator pseudo-elements,
  the status bar, and the keyboard never produce findings by default
  (`--include-system-chrome` restores them; individual keyboard keys stay
  under the dense-group rule), and `touch_target` withholds small
  controls covered by a full-size tappable list row (Cell or Button) —
  stock Settings' ~28pt row buttons — reported as `suppressed_covered_controls`
  (`--include-covered-controls` restores them). So a noisy stock-app
  screen audits clean; remaining findings are the app's own layout.
- Both artifacts land in the journal: `manzanas journal export $LID -o
  evidence.md` includes them — paste into the PR.

## Target elements with predicates, not coordinates

Never compute tap coordinates from a describe-ui dump. Use matchers:

```sh
manzanas tap-element --lease $LID --label "Continue"     # flat matcher, best-ranked
manzanas tap-element --lease $LID --predicate '{
  "type": "Button", "text": "Delete",
  "near": {"predicate": {"text": "Alice"}, "direction": "right"}}'
```

Predicate fields: `text` / `text_contains` / `text_regex` (one text form),
`type`, `accessibility_id`, `bounds_hint` (top_half/bottom_half/...),
`near` ({predicate, direction, max_distance?}), `parent_of` (predicate on a
descendant), `index` (explicit 0-based disambiguator). Prefer predicates
whenever a screen repeats controls (one Delete per row) — a flat matcher
would silently pick the "best" one.

If a matcher misses, call `ui_tree` (`manzanas observe`) to see what is
actually on screen, then tighten — do not fall back to coordinates.
Timeout errors now tell you when matching elements exist off-screen
(`... exist off-screen: Button label="Save" (below the viewport) —
scroll to bring them into view`): that means `scroll_to_element`, not a
new matcher. If instead the error says candidates are `on screen but
outside the requested in_frame/bounds_hint region`, fix the region —
scrolling will not help.

## Keep ui_tree cheap on busy screens

```sh
manzanas observe --lease $LID --compact                  # one line per element, [i] indexes
manzanas observe --lease $LID --interactive-only --exclude-system-chrome
manzanas observe --lease $LID --roles Button,Cell --scope '{"type":"Table"}'
```

MCP `ui_tree` takes the same fields (`format:"compact"`,
`interactive_only`, `roles`, `scope`, `exclude_system_chrome`). Filters
compose; `hash` always digests the full tree, so change detection is
unaffected. The compact `[i]` indexes are stable depth-first positions —
the same order the predicate `index` field uses.

## Correlate app logs with actions

Add `--capture-logs [--log-process MyApp]` (payload `capture_logs` /
`log_process`) to `tap-element`, `type-into-element`, or
`scroll-to-element` to capture the simulator's os_log lines emitted
during the action window. They come back as `result.logs` and are
journaled as `action-logs.txt` next to the step, so the journal export
shows what the app logged at each action.

## QA loop recipe

1. `lease_acquire` → launch the app → `wait_tree_stable`.
2. `audit` the screen; note refs worth acting on.
3. Drive to the next screen with `tap_element`/predicates; repeat.
4. `journal export $LID -o evidence.md` and attach findings + annotated
   screenshots to the PR.

Full docs: `docs/mcp.md` (audit + predicate DSL sections).
