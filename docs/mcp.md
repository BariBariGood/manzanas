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
| `ui_tree` | Compact structured accessibility tree (roles, labels, values, ids, frames, interactability) plus a stable tree hash for cheap change detection. The source of truth for matchers. |
| `tap_element` | Find an element by matcher and tap it, polling until it appears. |
| `type_into_element` | Find an element, tap to focus, and type text into it in one call. |
| `scroll_to_element` | Scroll (bounded swipes) until the matched element is visible on screen; distinguishes "never in the tree" from "matched but stuck off screen". |
| `wait_for_element` | Wait until a matching element appears (or disappears with `absent`). |
| `wait_tree_stable` | Wait until the screen stops changing (animations/loads settled). |

Matchers, shared by all element tools: `label` (visible text, substring),
`id` (accessibility identifier / testID, exact), `role` (Button, Cell,
TextField, ..., exact), `value` (current value, substring), `placeholder`
(placeholder text — the best way to find empty text fields), `exact`
(require exact text matches). At least one is required. When several
elements match, the daemon ranks exact text over substring hits,
interactable elements over static text and containers, and smaller frames
over larger, so the actual control beats a container whose label happens
to contain the text.

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
