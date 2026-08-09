# MCP server

`manzanas mcp` serves the manzanasd toolset over the Model Context
Protocol (stdio transport), so any MCP-capable agent — Claude Code,
Cursor, Codex, and friends — can lease and drive iOS simulators without
knowing the wire protocol.

The server is a thin facade: every tool call proxies to the configured
daemon (or broker), and any lease acquired through a session is
auto-released when the session ends, so crashed or disconnected agents
never orphan a simulator.

## Setup

You need two things: a running `manzanasd` daemon (on a Mac; see
[install.md](install.md) / [quickstart.md](quickstart.md)) and the
`manzanas` binary on the machine running your agent (single static Go
binary, cross-platform):

```sh
go build -o manzanas ./cmd/manzanas     # or brew install / release tarball
```

Point the client at the daemon with either mechanism — both are
expressible in every MCP client config:

- `MANZANASD_ADDR` environment variable (e.g. `mac-host:7433`), or
- the `--daemon` global flag: `manzanas --daemon mac-host:7433 mcp`.

If neither is set, it defaults to `127.0.0.1:7433`. To lease across a
multi-Mac fleet, point at a `manzanas-broker` endpoint instead
([broker.md](broker.md)).

`manzanas mcp` health-checks the daemon at startup and exits with an
actionable error if it is unreachable, so a misconfigured address
surfaces as one clear message in your MCP client's logs instead of every
tool call failing later. Sanity-check the connection yourself with:

```sh
manzanas --daemon mac-host:7433 targets
```

## Client configuration

Ready-to-paste configs. Replace `/path/to/manzanas` with the absolute
path to the binary (`which manzanas`) and `mac-host:7433` with your
daemon or broker address.

### Claude Code

```sh
claude mcp add manzanas -e MANZANASD_ADDR=mac-host:7433 -- /path/to/manzanas mcp
```

Or in a project-scoped `.mcp.json`:

```json
{
  "mcpServers": {
    "manzanas": {
      "command": "/path/to/manzanas",
      "args": ["mcp"],
      "env": { "MANZANASD_ADDR": "mac-host:7433" }
    }
  }
}
```

### Cursor (`.cursor/mcp.json`)

```json
{
  "mcpServers": {
    "manzanas": {
      "command": "/path/to/manzanas",
      "args": ["mcp"],
      "env": { "MANZANASD_ADDR": "mac-host:7433" }
    }
  }
}
```

### Codex (`~/.codex/config.toml`)

```toml
[mcp_servers.manzanas]
command = "/path/to/manzanas"
args = ["mcp"]

[mcp_servers.manzanas.env]
MANZANASD_ADDR = "mac-host:7433"
```

The `--daemon` flag works everywhere too:
`"args": ["--daemon", "mac-host:7433", "mcp"]`.

## Tools

Every tool that touches a target takes a `lease_id` from `lease_acquire`.
Leases owned by the MCP session are auto-released when the session ends.

Fleet and lifecycle:

| tool | what it does |
|---|---|
| `targets` | List simulators/devices and their states. |
| `lease_acquire` | Reserve a booted target; returns the `lease_id` every other tool needs. |
| `lease_renew` | Extend a lease's TTL before it expires. |
| `lease_release` | Release a lease when done. |
| `app` | Install / launch / terminate an app on the leased target. |
| `state` | Snapshot / restore / fixture deterministic simulator state. |
| `journal_export` | Export a run's journal as PR-ready evidence (`run_id` = `lease_id`): markdown summary with failures highlighted and artifact refs, or the raw JSON export with `format: "json"`. Works after the lease is released. |

Semantic element tools (preferred — the daemon matches accessibility
elements and acts on them, so the agent never computes coordinates):

| tool | what it does |
|---|---|
| `ui_tree` | Compact structured accessibility tree (roles, labels, values, ids, frames, interactability) plus a stable tree hash for cheap change detection. The source of truth for matchers. Trim big screens with `format:"compact"` (one line per element with stable `[i]` indexes), `interactive_only`, `roles`, `scope` (one subtree by predicate), or `exclude_system_chrome`; the hash always digests the full tree. |
| `tap_element` | Find an element by matcher and tap it, polling until it appears. |
| `type_into_element` | Find an element, tap to focus, and type text into it in one call. |
| `scroll_to_element` | Scroll (bounded swipes) until the matched element is visible on screen; distinguishes "never in the tree" from "matched but stuck off screen". |
| `wait_for_element` | Wait until a matching element appears (or disappears with `absent`). |
| `wait_tree_stable` | Wait until the screen stops changing (animations/loads settled). |
| `audit` | Deterministic UI-quality checks over the current tree → findings (evidence, not verdicts) + annotated screenshot, both journaled. See [The audit tool](#the-audit-tool-deterministic-ui-checks). |

Matchers, shared by all element tools: `label` (visible text, substring),
`id` (accessibility identifier / testID, exact), `role` (Button, Cell,
TextField, ..., exact), `value` (current value, substring), `placeholder`
(placeholder text — the best way to find empty text fields), `exact`
(require exact text matches). At least one is required. When several
elements match, the daemon ranks exact text over substring hits,
interactable elements over static text and containers, and smaller frames
over larger, so the actual control beats a container whose label happens
to contain the text.

## The audit tool (deterministic UI checks)

`audit` inspects one accessibility-tree observation with deterministic
geometry checks and returns **findings** — measured evidence, never
pass/fail verdicts. Run it instead of eyeballing screenshots or
hand-measuring `ui_tree` frames; you (the agent) decide which findings
matter for the app under test.

Checks (select a subset with `checks`, default all):

| check | evidence produced |
|---|---|
| `touch_target` | interactive elements smaller than `min_touch_pt` (default 44x44pt) in either dimension |
| `clipping` | frames extending beyond the screen bounds or beyond a non-scrolling parent's bounds, with per-edge overflow in points |
| `alignment` | sibling edges almost-but-not-quite aligned: deltas within `alignment_tolerance_pt` (default 4pt) — larger deltas are treated as intentional layout |
| `spacing` | gaps in sibling rows/columns deviating from the group's median by more than `spacing_tolerance_pt` (default 4pt), with the full gap list |
| `safe_area` | interactive elements intruding into the safe-area insets (`safe_area_insets` override, or a device-class heuristic) |
| `missing_labels` | interactive elements with no label, value, or placeholder — nothing a screen reader or an element matcher can use |

Each finding carries the check name, a `ref` (`F1`, `F2`, ...), the
element's role/label/id/frame, the measured values, and an evidence
sentence. The same refs are drawn as labeled red boxes on an annotated
screenshot. Both the findings JSON and the annotated PNG are journaled as
run artifacts and appear in `journal_export`, so audit evidence survives
into PR-ready exports.

Noise control: dense grids of repeated same-size controls (keyboards,
emoji pickers, calendar day cells) are detected and suppressed
(`suppressed_elements` reports how many); findings are capped per check
with an `elided` count when a screen is pathological. System chrome the
OS draws — scroll-indicator pseudo-elements (UIKit-labeled "Vertical/Horizontal
scroll bar…" Slider/ScrollBar nodes, plus thin unlabeled elongated
ones), the status bar, and the keyboard — is also suppressed by default
(`include_system_chrome: true` restores it, though individual keyboard
keys remain under the dense-group rule), and `touch_target` withholds
small controls whose frame is fully covered by an enclosing ≥min-size
tappable Cell or Button row (stock Settings' ~28pt row buttons and
chevrons — the row itself is the touch target; count reported as
`suppressed_covered_controls`, restore with
`include_covered_controls: true`). The heuristics are role/id/frame
based and deterministic — see `internal/actions/chrome.go`.

Scoping: pass any element matcher (flat fields or `predicate`) to audit
only the matched element's subtree, or `region` (`{x, y, w, h}` in
points) to audit only elements centred in that rectangle.

```jsonc
// audit just the visible form, tighter touch-target rule
{"lease_id": "lse_...", "checks": ["touch_target", "missing_labels"],
 "label": "Sign up", "min_touch_pt": 48}
```

CLI equivalent: `manzanas audit --lease lse_... [-o annotated.png]
[--checks touch_target,clipping] [--region X,Y,W,H] [matcher flags]
[--include-system-chrome] [--include-covered-controls]`.

## Trimming ui_tree on busy screens

`ui_tree` accepts opt-in filters that cut payload size server-side; they
compose, ordering stays deterministic (depth-first), and `hash` always
digests the full unfiltered tree:

```jsonc
// one line per element, only interactive ones, app content only
{"lease_id": "lse_...", "format": "compact",
 "interactive_only": true, "exclude_system_chrome": true}
// just the buttons inside one table
{"lease_id": "lse_...", "roles": ["Button"], "scope": {"type": "Table"}}
```

`format: "compact"` returns `tree_compact` — one depth-indented line per
element (`[12] Button "Save" (20,100 100x44) interactive`) with a stable
depth-first `[i]` index over the returned tree, the same order in which
the predicate `index` field picks candidates. When filters or `scope`
are active, `total_elements` / `returned_elements` report the reduction.
CLI: `manzanas observe --compact --interactive-only --roles Button,Cell
--scope '{"type":"Table"}' --exclude-system-chrome`.

## Log correlation (capture_logs)

`tap_element`, `type_into_element`, and `scroll_to_element` accept
`capture_logs: true` (plus optional `log_process: "MyApp"`) to collect
the simulator's `os_log` lines emitted during the action window. The
lines come back as `result.logs` and are journaled as an
`action-logs.txt` artifact next to the step's journal entry, so a
journal export correlates what the app logged with each action.
Best-effort (a collection failure degrades to `log_error`) and
simulator-only. CLI: `--capture-logs [--log-process NAME]`.

## The predicate DSL (strict matching)

Every element tool also accepts a `predicate` object — a strict,
composable alternative to the flat matcher fields above (use one form
per call, never both). Where the flat matcher silently picks the
best-ranked hit, a predicate must resolve to **exactly one** element:
several matches without an `index` fail with `ambiguous_match` and a
listing of every candidate (role, label, id, frame), and zero matches
poll until `timeout_ms` like any other matcher miss. Prefer predicates
when a screen has repeated controls (two "Delete" buttons, one per row)
and a silent best-match pick would be a guess.

Fields (all optional, at least one required; `index` alone is not a
selector):

| field | matches |
|---|---|
| `text` | the element's visible text (label or value), **exact** |
| `text_contains` | visible text, substring |
| `text_regex` | visible text, RE2 regular expression |
| `type` | the element type/role, exact (`Button`, `Cell`, `TextField`, ...) |
| `accessibility_id` | the accessibility identifier / testID, exact |
| `bounds_hint` | where the element's centre sits on screen: `top_half`, `bottom_half`, `left_half`, `right_half`, or `center` (the middle half in both axes) |
| `near` | `{predicate, direction, max_distance?}` — the element lies `left`/`right`/`above`/`below` of another **uniquely**-resolved element, overlapping its row/column, optionally within `max_distance` points centre-to-centre |
| `parent_of` | a predicate on a descendant; resolves the enclosing element — the direct parent by default, or the matching ancestor when combined with other fields (e.g. `type: "Cell"`) |
| `index` | 0-based pick among the remaining matches, in tree order — the explicit disambiguator |

`text`/`text_contains`/`text_regex` are mutually exclusive. `near` and
`parent_of` nest full predicates (up to 4 levels); an ambiguous inner
predicate fails the whole resolution with the same candidate listing.

```jsonc
// The Delete button in Alice's row, not Bob's:
{"predicate": {"type": "Button", "text": "Delete",
               "near": {"predicate": {"text": "Alice"}, "direction": "right"}}}

// The cell that contains the text "Alice":
{"predicate": {"type": "Cell", "parent_of": {"text": "Alice"}}}

// The second "Save" button (explicitly, not silently):
{"predicate": {"text": "Save", "index": 1}}

// The text field in the top half of the screen:
{"predicate": {"type": "TextField", "bounds_hint": "top_half"}}
```

On `ambiguous_match`, read the candidate listing in the error, then
either tighten the predicate (add `type`, `accessibility_id`,
`bounds_hint`, or `near`) or pick a candidate with `index`.

Raw tools (fallbacks when there is no usable accessibility element):

| tool | what it does |
|---|---|
| `observe` | Same tree + hash as `ui_tree` (kept for backward compatibility). |
| `tap` / `swipe` | HID input at raw coordinates. |
| `type_text` | Type into whatever currently has focus. |
| `button` | Press a hardware button (home, lock, ...). |
| `screenshot` | Capture the screen (returns PNG). |
| `record_start` / `record_stop` | Screen recording. |

## Post-action change signal

`tap_element`, `type_into_element`, and `scroll_to_element` results carry
`ax_before` / `ax_after` (accessibility tree hashes taken around the
action) and a derived `ui_changed` field, so an agent can tell "the tap
did something" without a follow-up screenshot. The daemon hashes the tree
best-effort and time-bounded, so under load a hash can be missing; when
either hash is unavailable `ui_changed` is `null` (unknown — fall back to
`ui_tree` if you need to confirm the effect). A `scroll_to_element` that
never swiped (element already visible) omits the signal entirely.
`scroll_to_element` also
returns `hash` (the tree hash of the final observation) and the matched
element's `frame`; `wait_tree_stable` returns the settled `hash`. Compare
hashes across calls to detect changes cheaply.

## Errors always say what to do next

A matcher that never matches within `timeout_ms` fails with the matcher
echoed back and a next step, e.g.:

```
element (label="Continue") did not appear within the 10s budget; call
ui_tree to see what is on screen now and adjust the matcher (or use
scroll_to_element if the element is further down the page)
```

An element that matched but sits outside the viewport suggests
`scroll_to_element`; `scroll_to_element` itself distinguishes an element
that was never in the tree (`timeout` — likely the wrong screen) from one
that matched but could not be brought into view (`off_viewport` — pinned
off-screen or in a non-scrollable container).

## Example transcript

A realistic agent session filling in a sign-up form ("fill in the e-mail
field on the settings screen and save"):

```
agent → lease_acquire {}
      ← {"id":"lse_9f2","state":"active","target_udid":"ABCD-...","ttl_seconds":300}

agent → app {"lease_id":"lse_9f2","action":"launch","bundle_id":"com.example.app"}
      ← {"ok":true}

agent → wait_for_element {"lease_id":"lse_9f2","label":"Settings"}
      ← {"element":{"role":"Button","label":"Settings","frame":{...}},"polls":2}

agent → tap_element {"lease_id":"lse_9f2","label":"Settings"}
      ← {"element":{...},"x":215,"y":780,"ax_before":"a1f...","ax_after":"9c2...","ui_changed":true}

agent → scroll_to_element {"lease_id":"lse_9f2","label":"Notifications"}
      ← {"element":{"role":"Cell","label":"Notifications","frame":{...}},"scrolls":2,"hash":"77b..."}

agent → type_into_element {"lease_id":"lse_9f2","placeholder":"Email","text":"me@example.com"}
      ← {"element":{...},"typed_runes":14,"ui_changed":true}

agent → tap_element {"lease_id":"lse_9f2","label":"Save"}
      ← error: element (label="Save") did not appear within the 10s budget;
        call ui_tree to see what is on screen now and adjust the matcher ...

agent → ui_tree {"lease_id":"lse_9f2"}
      ← {"tree":[... {"role":"Button","label":"Done","id":"save-button"} ...],"hash":"d41..."}

agent → tap_element {"lease_id":"lse_9f2","id":"save-button"}
      ← {"element":{...},"ui_changed":true}

agent → lease_release {"lease_id":"lse_9f2"}
      ← {"released":true}
```

When a matcher times out but elements matching it exist outside the
viewport, the error appends an off-screen hint — e.g. `2 matching
element(s) exist off-screen: Button label="Save" ... (below the
viewport) — scroll to bring them into view (scroll_to_element)` — and
ambiguous DSL matches mark off-screen candidates with `(off-screen)`.
Candidates that are on screen but excluded by the matcher's own
`in_frame`/`bounds_hint` are reported separately (`... on screen but
outside the requested in_frame/bounds_hint region ... — relax or adjust
the region`), so scroll advice only appears when scrolling can help.

Note the recovery pattern: a matcher miss is not a dead end — `ui_tree`
shows what is actually on screen, and the `id` matcher is the most
reliable when the app sets accessibility identifiers.

## Troubleshooting

**"daemon health check failed" at startup** — the daemon is down or the
address is wrong. Check what the server is pointed at (the error prints
the current address), verify the daemon from the same machine:

```sh
curl -s http://mac-host:7433/v0/healthz
```

and fix `MANZANASD_ADDR` / `--daemon` in your client config. Remember the
address is resolved on the machine running the MCP server (your agent's
machine), not the Mac.

**Server starts but the client shows no tools** — MCP stdio servers must
keep stdout pure JSON-RPC. `manzanas mcp` logs only to stderr; if you
wrapped it in a shell script, make sure the wrapper doesn't print to
stdout. Verify the handshake by hand:

```sh
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | manzanas mcp
```

or interactively with the MCP inspector:

```sh
npx @modelcontextprotocol/inspector -e MANZANASD_ADDR=mac-host:7433 /path/to/manzanas mcp
```

**Tool calls fail with "no active lease" / "lease expired"** — every
driving tool needs a `lease_id` from `lease_acquire`. Leases have a TTL
(default 300 s); long sessions should call `lease_renew` before expiry.
The daemon allows a short renewal grace window after nominal expiry —
actions and a late renew may still succeed for a couple of minutes — but
don't rely on it. Tool errors include a what-to-do-next hint for exactly
this reason.

**Calls hang in `lease_acquire`** — all matching targets are busy and the
lease is queued (the default `wait: true` blocks until grant). Pass
`wait: false` to return immediately, or check queue depth with
`manzanas lease ls`.

**Daemon returns 503 "overloaded"** — the host hit a safety gate (too
many booted sims, memory pressure). Back off and retry; see
[troubleshooting.md](troubleshooting.md).
