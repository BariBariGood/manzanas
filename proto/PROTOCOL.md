# manzanasd wire protocol — v0

Status: draft, versioned. Breaking changes bump the version; `v0` may change
until v0.1 ships, after which changes must be additive.

manzanasd exposes two surfaces on one port (default `:7433`):

1. **HTTP REST** for non-streaming request/response ops (everything below).
2. **JSON-over-WebSocket** at `GET /v0/ws` for the same ops plus
   server-initiated events (lease grants, target state changes) and, later,
   media streams.

All JSON types are defined in the Go package
`github.com/BariBariGood/manzanas/proto` — that package is the source of
truth; this document describes semantics.

## 1. Conventions

- All paths are prefixed with the protocol version: `/v0/...`.
- Errors: non-2xx HTTP responses (and WS `error` fields) carry
  `{"code": "...", "message": "..."}`. Well-known codes: `not_found`,
  `not_implemented`, `bad_request`, `lease_expired`, `no_match`,
  `target_busy`, `target_not_booted`, `stream_limit`, `viewer_limit`,
  `stream_gone`, `timeout`, `off_viewport`, `ambiguous_match` (`409` — a
  structured predicate matched several elements and no `index` picked
  one; the message lists the candidates), `focus_required` (`412` — a
  typing action with `require_focus:true` found no focused text field),
  `internal`. Two additive
  optional fields: `detail` carries actionable context when the daemon
  has any (e.g. a `target_not_booted` error against a leased target
  reports who shut the target down and why, or that the daemon has no
  record of doing so), and `retry_after_seconds` is a machine-readable
  retry hint on transient refusals (`overloaded`), mirrored in an HTTP
  `Retry-After` header.
- Timestamps are RFC 3339 UTC.
- `GET /v0/healthz` returns `200 {"ok":true,"version":"v0"}`.
- `GET /v0/status` returns the daemon's load/occupancy snapshot for fleet
  schedulers (`HostStatus`): `capacity` (`max_booted_running`,
  `max_parked`, `max_concurrent_boots`; zeros = no warm pool/uncapped),
  `running` (Booted, un-parked sims, served from a short-lived cache),
  `unmanaged_sims` (optional/additive: Booted, un-parked sims the daemon
  neither pools nor booted itself — another agent's sims; the daemon's
  janitor leaves these alone unless opt-in stale-lock reaping is on),
  `reaped_sims` (optional/additive: unmanaged sims the janitor has shut
  down — never deleted — because their fleet lock file was stale or
  missing; only non-zero with stale-lock reaping opted in),
  `parked`, `boots_in_flight`, `leases_active`, `leases_queued`,
  `load_avg1`, `cpus`, `free_disk_bytes`, and `gates`
  (`load_ok`/`disk_ok` — a false gate means a cold boot would be refused
  right now). When advice has been received (below), the snapshot also
  carries `pool_advice` (`PoolAdviceState`: `source`, `received_at`,
  `window_seconds`, `classes`). Additive: schedulers must tolerate older
  daemons answering `404`.
- `POST /v0/pool/advise` records a fleet scheduler's advisory view of
  warm-pool demand (`PoolAdviseRequest`): `source`, `window_seconds`, and
  `classes` — each a `PoolClassAdvice` with `labels` (the demand class),
  `action` (`grow`|`shrink`), and optional `cold_placements`/`warm_hits`
  counts and a human-readable `reason`. Validation: an unknown `action`
  is `400 bad_request`; `grow` requires non-empty `labels` (`shrink` with
  empty labels applies to the pool as a whole). Returns
  `200 {"accepted":true,"acted":false}` — **advice is never binding**:
  the daemon only records the latest advice (surfaced as `pool_advice`
  on `GET /v0/status`) and keeps final say over its warm pool via its
  own capacity class and safety gates. Additive: older daemons answer
  `404`, which advising schedulers must tolerate.

## 2. Target enumeration

- `GET /v0/targets` → `{"targets": [Target, ...]}`

`Target`: `udid`, `kind` (`simulator`|`device`), `name`, `runtime`,
`device_type`, `state` (`Shutdown`|`Booted`|`Booting`|`ShuttingDown`|`Unknown`),
`labels`.

A `Target` served by a federating broker (see [docs/broker.md](../docs/broker.md))
additionally carries `host` — the owning fleet host's name; a single
daemon leaves it empty.

A daemon with a warm pool marks parked targets `"warm": true` — booted
but SIGSTOPped, thawed in ~26ms when a lease grants — so schedulers can
prefer them over cold boots. The field is omitted otherwise.

A daemon with video recording wired marks targets with a live recording
`"recording": true`; the field is omitted otherwise.

Labels are derived by the registry from runtime and device type, lowercased
and hyphenated, e.g. an "iPhone 17 Pro" on "iOS 26.5" gets
`["simulator", "ios26", "ios26.5", "iphone-17-pro"]`.

Boot / shutdown (requires an active lease on the target):

- `POST /v0/targets/{udid}/boot` with `{"lease_id": "..."}` → `202 {Target}`
- `POST /v0/targets/{udid}/shutdown` with `{"lease_id": "..."}` → `202 {Target}`

Boot is asynchronous; poll `GET /v0/targets` (or watch `target.state` events
on the WS) until `state == "Booted"`.

On real (darwin, non-mock) daemons every boot passes host safety gates
before it is accepted: a running-sim cap (capacity class: 2 on Intel,
4 on Apple Silicon; parked pool sims don't count), a load gate (refuse
when 1-min load > 2× cores) and a free-disk gate (refuse below 5 GiB).
A gate refusal answers `503 {"code":"overloaded"}` with a `Retry-After`
header and a matching `retry_after_seconds` field in the body — wait at
least that long before retrying instead of busy-polling. Alternatively,
boot with `?wait=true` (`POST /v0/targets/{udid}/boot?wait=true`): the
daemon then retries a gate-refused boot server-side (polling every few
seconds, up to a 10-minute budget) and answers `202 {Target}` once the
boot is accepted; if the budget (or the request context) runs out first,
the last refusal is returned as the usual hinted `503`. The lease is
re-validated on every retry — if it expires or loses the target while
waiting, the wait aborts with `410 lease_expired`. Concurrent waiters
are capped per daemon *and* per lease (one wait per lease at a time);
excess `?wait=true` requests get the hinted `503` immediately.
A boot request may also surface `500` with the failure of a *previous*
accepted boot that later died in the background; retrying proceeds
normally. The gates are tunable on the daemon via `--pool-max-running`,
`--pool-load-factor` and `--pool-min-free-disk-gb` (0 = default,
negative disables that gate).

Video recording (requires an active lease on the target; see
[docs/recording.md](../docs/recording.md) for lifecycle detail):

- `POST /v0/targets/{udid}/recording` body `RecordingRequest`
  (`lease_id`, `codec?` — `hevc` (default) or `h264`, `max_seconds?` —
  clamped to the daemon cap) → `201 {Recording}` (`recording_id`, `udid`,
  `codec`, `max_seconds`, `started_at`). The target must be `Booted`
  (`recordVideo` against a shut-down sim exits 0 with a 0-byte file); a
  non-booted target, an already-recording target, or a poisoned recording
  session → `409 {"code":"target_busy"}`. Concurrency cap or free-disk
  floor exceeded → `503 {"code":"overloaded"}`. Daemons without a journal
  → `501 {"code":"not_implemented"}`.
- `POST /v0/targets/{udid}/recording/stop` body `RecordingStopRequest`
  (`lease_id`) → `200 {RecordingStopResult}` (`recording_id`, `artifact?`
  — path/sha256/bytes, `journal_ref?`, `codec`, `duration_s`, `bytes`,
  `reason`). No live recording (or a concurrent stop won) →
  `409 {"code":"target_busy"}`; a recording that failed validation →
  `422 {"code":"internal"}`.

The finalized mp4 lands in the run journal as a `kind:"segment"` /
`recording.stop` entry plus a content-addressed artifact downloadable via
`GET /v0/journal/{run}/artifacts/{path}`. Recordings also auto-stop with a
matching `reason` on lease end, target shutdown, duration/size caps, and
daemon shutdown/restart. `AcquireLeaseRequest` accepts an additive
`record:"video"` field (echoed on the `Lease`) to auto-record the whole
lease from boot to lease end.

### 2.1 Dashboard controls (`/v0/dash/`)

Operator endpoints backing the built-in web dashboard (see
[docs/dashboard.md](../docs/dashboard.md)). They exist because lease IDs
are capability tokens the dashboard never holds: targets are addressed by
UDID instead. Agents should keep using the lease-scoped endpoints above.

- `GET /v0/dash/config` → `{"readonly": bool}` — whether the daemon runs
  with `--dash-readonly`.
- `POST /v0/dash/targets/{udid}/boot` / `.../shutdown` → `202 {Target}` —
  leaseless lifecycle ops on a **free** target only: a target held by an
  active lease, mid post-lease reset or warm-pool rebuild, or parked in
  the warm pool → `409 {"code":"target_busy"}` (the holder / the pool
  owns its lifecycle). Boots pass the same host
  safety gates as leased boots.
- `POST /v0/dash/targets/{udid}/release` → `200 {Lease}` (ID redacted) —
  releases whatever active lease holds the target; its post-lease reset
  still runs. No active lease → `404`.
- `POST /v0/dash/targets/{udid}/recording/stop` →
  `200 {RecordingStopResult}` — stops the target's live recording
  (whichever run started it) and ingests it. No live recording → `404`.

With `--dash-readonly` every mutation above answers
`403 {"code":"read_only"}`.

## 3. Leases

A lease is a TTL-bounded exclusive claim on one target. Every mutating
operation on a target requires an active lease.

- `POST /v0/leases` body `AcquireLeaseRequest`
  (`labels`, `udid?`, `agent_id`, `purpose?`, `ttl_seconds?` — default 300,
  max 3600). `agent_id` identifies the caller and is required;
  `session_id` is accepted as an alias (it fills `agent_id` when that
  field is empty; `agent_id` wins when both are set, and the `Lease`
  always echoes the resolved value as `agent_id`). A request with
  neither field is `400 {"code":"bad_request"}`.
  `udid` pins the request to that specific target: only that
  target can satisfy the lease, and it must also match `labels` (if any).
  The pin is echoed back on the `Lease` as `requested_udid`.
  - If a matching target is free → `201 {Lease}` with `state:"active"`,
    `target_udid`, `expires_at`.
  - If all matching targets are leased → `202 {Lease}` with
    `state:"queued"` and a 1-based `queue_position`. The queue is FIFO per
    matching criteria (label-set plus pinned `udid`, if any). Poll
    `GET /v0/leases/{id}` (or WS `leases.get`) until active;
    `lease.granted` events may arrive sooner, but queued holders MUST still
    poll at least every 30 minutes to keep their place in the queue.
  - If no target matches the labels (or the pinned `udid` is unknown or
    fails the labels) → `409 {"code":"no_match"}`.
- `GET /v0/leases` → `200 {"leases": [Lease, ...]}` — all known leases
  (active, queued, and recently terminal), newest first, with their `id`s
  (the journal endpoints expose lease IDs as run IDs anyway, so listing
  them keeps the entries correlatable with `GET /v0/leases/{id}` and
  journal runs). Listing does not keep a queued lease alive; only
  `GET /v0/leases/{id}` does.
- `GET /v0/leases/{id}` → `200 {Lease}`
- `POST /v0/leases/{id}/renew` body `RenewLeaseRequest` → `200 {Lease}`
  (only active leases can renew; expired → `410 {"code":"lease_expired"}`).
  Renewal is allowed through the grace window (below): a renew that
  arrives after nominal `expires_at` but before `grace_until` succeeds,
  clears `grace_until`, and re-arms the lease with the new TTL.
- `DELETE /v0/leases/{id}` → `200 {Lease}` with `state:"released"`.
  Releasing (or expiry) hands the target to the head of the matching queue.
  Release is idempotent: for a lease already in a terminal state
  (`expired` or `released`) it is a no-op and returns the lease with its
  current terminal state unchanged.

A `Lease` served by a federating broker additionally carries `host` and
`host_addr` (the owning daemon's base URL): clients should perform all
target-bound ops (boot, actions, streams, state) against `host_addr`
directly. See [docs/broker.md](../docs/broker.md).

Expiry and the renewal grace window: when an active lease passes its
nominal `expires_at`, it does **not** expire immediately. It first enters
a grace window (daemon-configurable via `--lease-renew-grace` /
`MANZANASD_LEASE_RENEW_GRACE`, default 2 minutes; 0 disables): the lease
stays `active`, the additive `grace_until` field is stamped on it, a
`lease.expiring` WS event fires once, and the post-lease reset/reclaim
and any queue promotion are deferred. Renewing during the window rescues
the lease as if it had never lapsed. Only when `grace_until` passes
without a renewal does the lease actually expire (state `expired`,
`lease.expired` event, reset runs, queue head promoted). Read endpoints
additionally stamp `expires_in_seconds` (derived, additive) on active
leases — negative inside the grace window. Holders should still renew at
~half TTL and treat the grace window as a last-chance safety net, not a
TTL extension. Queued leases also expire if their owner
does not poll `GET /v0/leases/{id}` (or WS `leases.get`) for 30 minutes, so
abandoned requests don't block the queue; watching events alone does not
keep a queued lease alive. Additive liveness rule for wait-polling owners:
once a queued lease's owner has polled `GET /v0/leases/{id}` at least once,
going silent for more than 30 seconds marks it abandoned — when a target
frees up, such a lease is expired instead of granted, so a client killed
mid-wait never has its queue entry go active and hold the target until its
TTL. Waiting clients (the CLI/MCP poll every ~2 s) are unaffected, and an
owner that has never polled (e.g. an acquire with `wait:false` that checks
back later) keeps the plain 30-minute contract above.

## 4. WebSocket surface

`GET /v0/ws` upgrades to a WebSocket carrying `Envelope` frames:

```json
{"v":"v0","id":"1","method":"leases.acquire","params":{"labels":["ios26"],"agent_id":"claude-1"}}
{"v":"v0","id":"1","result":{"id":"lse_...","state":"active","target_udid":"..."}}
{"v":"v0","event":"lease.granted","result":{"id":"lse_...","target_udid":"..."}}
```

Methods mirror REST 1:1: `targets.list`, `targets.boot`, `targets.shutdown`,
`leases.acquire`, `leases.get`, `leases.renew`, `leases.release`,
`actions.dispatch`, `actions.batch`, `streams.open`. Params/results are the
same JSON bodies as REST. Events: `lease.granted`, `lease.expiring`
(an active lease entered its renewal grace window — renew now or it
expires for real at `grace_until`), `lease.expired`, `target.state`.

Action dispatch (`actions.dispatch` / `actions.batch`) is handled off the
connection's read loop, so a long-running action (e.g. a `wait_*` poll)
never blocks lease renewals or other requests on the same connection.
Pipelined actions on one connection still execute strictly in the order
they were sent (they share a single per-connection worker); only a small
number may be queued, beyond which the server answers `overloaded`
immediately. This ordering guarantee covers action methods only: other
methods (`leases.*`, `targets.*`, `state.*`) run inline on the read loop
and may overtake a queued action — a client must await an action's
response before, e.g., releasing the lease it runs under.

## 5. Action dispatch (owned by the actions slice)

Every action goes through one request envelope: `POST /v0/actions` body
`ActionRequest` with exactly these top-level fields —

- `lease_id` — the active lease whose target the action runs on;
- `kind` — the action name (e.g. `tap`, `tap_element`, `screenshot`;
  see the tables in §5.1);
- `payload` — a JSON object with the per-kind fields from those tables.
  The payload is **opaque** to the core; its schema is owned by
  `internal/actions`.

The response is `200 {ActionResult}`: `{"ok": bool, "result": {...},
"journal_ref"?: {...}}` — the per-kind result fields in the §5.1 tables
live **inside `result`**, never at the top level.

Worked example — tap the element labelled "Continue", then capture a
small JPEG screenshot:

```sh
curl -s -X POST http://127.0.0.1:7433/v0/actions -d '{
  "lease_id": "lse_abc123",
  "kind": "tap_element",
  "payload": {"label": "Continue", "timeout_ms": 10000}
}'
# → {"ok":true,"result":{"element":{...},"x":215,"y":640,"elapsed_ms":312,"polls":1},
#    "journal_ref":{"run_id":"lse_abc123","seq":4}}

curl -s -X POST http://127.0.0.1:7433/v0/actions -d '{
  "lease_id": "lse_abc123",
  "kind": "screenshot",
  "payload": {"format": "jpeg", "max_dim": 800}
}' | jq -r .result.jpeg_base64 | base64 -d > shot.jpg
# the image is result.jpeg_base64 (result.png_base64 for PNG) — inside
# `result`, not top-level
```

Other rules:

- The target is the one held by `lease_id`; there is no separate `udid`
  field. A missing/unknown lease is `404 not_found`, a non-active lease is
  `410 lease_expired`. Terminal (released/expired) leases stay resolvable
  for the manager's retention window (10 minutes); after that they are
  garbage-collected and indistinguishable from unknown leases
  (`404 not_found`). Builds with no actions backend wired return
  `501 not_implemented`.
- Backend errors map to `400 bad_request` (bad payload / unknown kind),
  `503 unavailable` (a required host tool such as AXe is not installed,
  or the a11y bridge stayed unready through the retry budget — retry
  later), `409 target_not_booted` (the leased target is shut down; boot
  it first — the error's `detail` field reports the daemon's record of
  who shut the target down and why, or that it has no such record and
  the shutdown was external), or `500 internal` (the underlying command
  failed).
- Optional boolean payload fields (`include_raw`, `inline`,
  `terminate_running`, `ax_hashes`) must be JSON booleans; other types
  are `400 bad_request`, never silently coerced.

### 5.0 Batch dispatch

- `POST /v0/actions:batch` body `BatchActionRequest`
  (`lease_id`, `actions` — ordered array of `{kind, payload}` entries, max
  32 — and `stop_on_error?`, default false) → `200 {BatchActionResult}`.
- Actions run strictly in order against the leased target. Each entry is
  validated, dispatched, and journaled exactly as if sent alone to
  `POST /v0/actions` (the lease is re-checked per action, since it can
  expire mid-batch).
- `BatchActionResult`: `ok` (true only if every action succeeded),
  `completed` (number of actions that ran), and `results` — one
  `{ok, result?, error?, journal_ref?}` per executed action, in order.
  With `stop_on_error:true` the batch halts after the first failing
  action, so `results` may be shorter than `actions`; without it every
  action runs and failures are reported per entry.
- A batch also carries a 5-minute wall-clock budget: entries still
  pending when it expires are not run, and the next slot in `results`
  reports a `timeout` error (so a batch of long `wait_*` actions cannot
  monopolize the action pipeline for stacked per-entry timeouts). That
  placeholder is not counted in `completed`, so callers can resume from
  `actions[completed:]`.
- Each entry accepts exactly two keys, `kind` and `payload`; any other
  top-level key (e.g. the common `params` typo) is `400 bad_request`
  naming the offending field. Nothing in the batch runs in that case —
  a misnamed payload is never silently dropped and dispatched with
  action defaults.
- The batch itself only fails wholesale (non-200) for request-level
  problems: an empty or oversized `actions` array, an entry with an
  unknown key (`400 bad_request`), or a missing actions backend
  (`501 not_implemented`). Per-action failures are reported inside
  `results` with the same error codes as single dispatch.
- WS method `actions.batch` mirrors this endpoint.

### 5.1 Action kinds (v0.1, simulators)

HID (AXe):

| `kind` | payload | result |
|---|---|---|
| `tap` | `x`, `y` (>= 0) | `x`, `y` |
| `swipe` | `start_x`, `start_y`, `end_x`, `end_y` (>= 0), `duration_seconds?` | echoes the path |
| `type` | `text` (chars AXe can map to HID keycodes; an unmappable char such as `\t` is `400 bad_request`, and any prefix before it may already have been typed), `strategy?` (`hid` default \| `paste`), `require_focus?` (default `false`) | `typed_runes`, `strategy?` (echoed when `paste`) |
| `button` | `name` ∈ `home`, `lock`, `side-button`, `siri`, `apple-pay` | `button` |
| `key` | `keycode` (HID usage code; integer in `[0, 2^32-1]`, else `400 bad_request`), `duration_seconds?` | `keycode` |
| `key_sequence` | `keycodes` (array; each an integer in `[0, 2^32-1]`, else `400 bad_request`) | `count` |
| `tap_element` | matcher (`label?`, `role?`, `value?`, `id?`, `placeholder?`, `exact?`, `match?`, `in_frame?` — or `predicate?`, see below), `anchor?` (`start` \| `center` (default) \| `end`), `refresh?`, `timeout_ms?` (default 10000, max 120000), `interval_ms?` (default 500, min 10) | `element` (matched node, no children), `x`, `y` (tapped point), `elapsed_ms`, `polls` |
| `type_into_element` | same matcher/timing/anchor fields plus `text`, `strategy?` (`hid` default \| `paste`), `require_focus?` (default `false`) | `tap_element`'s fields plus `typed_runes`, `strategy?` (echoed when `paste`) |
| `scroll_to_element` | matcher (`label?`, `role?`, `value?`, `id?`, `placeholder?`, `exact?`, `match?`, `in_frame?` — or `predicate?`), `anchor?`, `direction?` (`down` (default) \| `up` \| `left` \| `right`), `max_scrolls?` (default 8, max 30), `refresh?`, `timeout_ms?` (default 30000, max 120000), `interval_ms?` (default 500, min 10) | `element` (matched node incl. `frame`, no children), `scrolls`, `polls`, `elapsed_ms`, `hash` (tree hash at success) |

`tap_element` and `type_into_element` are composite actions: the server
observes the a11y tree, finds the best element matching the predicate
(same semantics and ranking as `wait_for_element`, including polling until
the element appears within `timeout_ms`), and taps it —
`type_into_element` then types `text` into the focused field — all in one
request, saving the caller a full observe round-trip per interaction. A
matcher that never matches is `408 timeout`; when elements matching the
predicate exist in the tree but sit outside the viewport (or outside the
matcher's `in_frame`/`bounds_hint` constraint), the timeout message
appends an off-screen hint listing up to 3 candidates in depth-first
order with their direction (`below the viewport`, ...), telling the
agent to `scroll_to_element` instead of concluding the element does not
exist. `wait_for_element` (appear) and `scroll_to_element` timeouts, and
the `audit`/`observe` scope matchers, carry the same hint; ambiguous DSL
matches mark listed candidates that are off-screen with `(off-screen)`.

Every element action alternatively accepts a `predicate` payload field —
a structured predicate object (additive, v0) that replaces the flat
matcher fields (combining the two forms is `400 bad_request`). Its
fields: `text?` (exact visible text — label or value), `text_contains?`
(substring), `text_regex?` (RE2) — at most one text form;
`type?` (element role, exact), `accessibility_id?` (exact),
`bounds_hint?` (`top_half` \| `bottom_half` \| `left_half` \|
`right_half` \| `center` — where the element's frame centre sits
relative to the viewport), `near?`
(`{predicate, direction: left|right|above|below, max_distance?}` — the
element lies in that direction from another uniquely-resolved element,
overlapping its row/column, within `max_distance` points centre-to-centre
when set), `parent_of?` (a predicate on a descendant; resolves the direct
parent, or the matching ancestor when other fields constrain it), and
`index?` (0-based pick among the matches, in depth-first tree order). At
least one field besides `index` is required; `near`/`parent_of` nest up
to 4 levels. Unlike the flat matcher there is no best-match ranking:
several matches without `index` are `409 ambiguous_match` with every
candidate listed (role/label/id/frame) in the message, and zero matches
keep polling until `408 timeout`. An `absent:true` wait treats an
ambiguous predicate as "still present" and keeps polling.

The tap lands on the element's frame centre by default. `anchor: "start"`
or `"end"` moves it to a point inset half the frame height from the
leading/trailing edge instead — for a text element whose label is longer
than the interesting part (an inline "Sign in" link at the end of a
sentence), the edge anchors land on the matched text instead of the middle
of the sentence.

A tap point outside the device viewport (the root a11y element's screen
bounds) is `409 off_viewport` instead of a silently ineffective tap;
scroll the element into view and retry. A match that is only transiently
off screen (a sheet still animating in) keeps the wait polling within
`timeout_ms`; `off_viewport` surfaces only when the budget expires with
no on-screen match. `refresh: true` forces every poll
to bypass the resident warm helper and observe cold (see `observe`).

`scroll_to_element` scrolls until the matched element's anchor point is
inside the viewport, with bounded swipe attempts (`max_scrolls`). While
the element is absent from the tree it swipes in `direction` (the edge of
the content to reveal: `down` reveals content below the fold); once the
element is in the tree but off-viewport, each swipe moves toward its
frame instead. Swipes travel 40% of the viewport's height (or width)
through its centre, so one swipe cannot fling far past the target. The
two failure modes are distinct: an element that never appeared in the
tree is `408 timeout`, while one that matched but could not be brought
into the viewport (pinned off-screen, non-scrollable container) is `409
off_viewport`. On success the result carries the element with its
`frame`, the swipe count, and the tree `hash` of the final observation
(cheap change detection without another `observe`).

Typing strategies (`type` and `type_into_element`):

- `strategy: "hid"` (default) synthesizes one hardware-keyboard event per
  rune — the historical behavior.
- `strategy: "paste"` copies `text` onto the simulator pasteboard
  (`simctl pbcopy`) and sends a single Cmd-V chord instead of
  per-keystroke events. Use it for long text, text with characters that
  have no HID keycode, and on slimmed sims: on iOS 26.5 a sim slimmed
  without the speech daemons (`com.apple.assistantd`,
  `com.apple.corespeechd`) wedges the frontmost app's **main thread** in
  `AFDictationConnection` after hardware-keyboard typing — paste avoids
  that path entirely. Tradeoffs: the simulator pasteboard is
  overwritten, per-keystroke handlers fire once for the whole text, and
  the focused field must accept paste. The chord runs through the warm
  helper's `key_combo` op when resident, else `axe key-combo` (AXe
  releases predating `key-combo` are reported as `503 unavailable` with
  an upgrade hint). Simulator-only: on devices (WDA already delivers
  text app-side without hardware-keyboard events) `strategy:"paste"` is
  `501 not_implemented`.

`require_focus: true` makes the typing action verify — before sending any
keystroke — that a text field actually has keyboard focus, by polling the
a11y tree (bounded, ~10 s — sized to cover several cold AXe observes) for
on-screen keyboard elements (`Key` /
`Keyboard` roles). When no keyboard appears the action fails with
`412 focus_required` instead of typing, so stray keys cannot land outside
a field (e.g. a stray `r` reaching Metro reloads a React Native app).
For `type_into_element` the check runs after the focusing tap. Default
`false` because a simulator with *Connect Hardware Keyboard* active never
shows the on-screen keyboard; opt in on headless QA sims.

HID and composite element actions (plus `launch_app`/`terminate_app`)
accept an optional `capture_logs: true` payload flag (simulators only):
after the action completes, the backend collects the simulator's
`os_log` lines emitted during the action window (1 s of lead before the
action start, via `simctl spawn <udid> log show`), optionally filtered
with `log_process` (process name), and returns them as `logs` (newest 64
KiB, older lines truncated). Collection is best-effort and time-bounded:
a failure degrades to a `log_error` field, never a failed action. On a
journaling daemon the lines are also stored as an `action-logs.txt` run
artifact on the step's journal entry, correlating app logs with journal
steps.

HID actions accept an optional `ax_hashes: true` payload flag: the backend
then hashes the a11y tree before and after the action (one extra
`describe-ui` each) and returns `ax_before`/`ax_after`, which the journal
records on the action's entry. Best-effort — a hash failure never fails the
action.

Observation:

| `kind` | payload | result |
|---|---|---|
| `observe` | `include_raw?`, `refresh?`, `format?` (`"json"` default \| `"compact"`), `interactive_only?`, `roles?` (array of role names), `scope?` (structured predicate object; must match exactly one element), `exclude_system_chrome?` | `tree` (compacted a11y nodes; replaced by `tree_compact` — one depth-indented line per element with stable depth-first `[i]` indexes — when `format:"compact"`), `hash` (always digests the FULL unfiltered tree), `total_elements?`/`returned_elements?` (when filters/scope active), `raw?`, `detail?` (`"empty_tree"` when the tree is empty) |
| `screenshot` | `inline?` (default true), `format?` (`png` default \| `jpeg`), `quality?` (JPEG only, 1–100, default 80), `max_dim?` (longer-edge pixel cap; the capture is downscaled server-side, aspect preserved) | `format`, `bytes`, `sha256`, `backend`, `png_base64?` / `jpeg_base64?` (key follows `format`) |
| `audit` | `checks?` (subset of `touch_target`, `clipping`, `alignment`, `spacing`, `safe_area`, `missing_labels`; default all), `region?` (`{x,y,w,h}` points), matcher fields or `predicate?` (scope to one subtree), `min_touch_pt?` (default 44), `alignment_tolerance_pt?` (default 4), `spacing_tolerance_pt?` (default 4), `safe_area_insets?` (`{top?,bottom?,left?,right?}`; default device-class heuristic), `inline?` (default true), `include_system_chrome?` (default false — system chrome such as the status bar, keyboard, and scroll-indicator pseudo-elements is suppressed from findings by default; see `internal/actions/chrome.go`), `include_covered_controls?` (default false — `touch_target` withholds small controls fully covered by an enclosing ≥min-size tappable Cell or Button row), `timeout_ms?`, `interval_ms?` | `findings` (array of `{check, ref, element{role?,label?,id?,frame?}, related?, measured?, evidence}` — measured evidence, no verdicts), `counts` (per check), `suppressed_elements`, `suppressed_covered_controls?`, `elided?` (per-check overflow past the 20-findings cap), `hash`, `viewport?`, plus the annotated screenshot as `format`/`bytes`/`sha256`/`backend`/`png_base64?` — or `screenshot_error?` when the capture failed (findings survive) |

`observe` runs `axe describe-ui` and compacts the result: each node carries
`role`, `label`, `value`, `placeholder`, `id`, `frame` (`x`,`y`,`w`,`h`),
`interactable`, `disabled`, `children`, with empty fields omitted and
information-free wrapper elements collapsed into their children. `hash` is a
stable digest of the tree so callers can cheaply detect UI changes between
observations. Tap a listed element at its frame centre
(`x + w/2`, `y + h/2`). The journal records `hash` as the entry's
`ax_after`.

`refresh: true` forces the snapshot to come from a freshly spawned AXe
process instead of the resident warm helper, whose long-lived
accessibility connection can occasionally serve a stale tree (e.g. a
frontmost modal/sheet missing while the background screen still shows);
it costs a process spawn per call and is a no-op when no warm helper is
configured (the cold path is always fresh).

An empty tree is retried inside the daemon (bounded, with backoff — the
a11y bridge on React Native screens intermittently serves an empty
snapshot while re-attaching). When it stays empty the result carries
`detail: "empty_tree"` alongside `tree: []` so callers can distinguish a
retryable blank snapshot from a settled screen and re-observe. The
`wait_*` actions already treat an empty snapshot as "not yet": it neither
matches a predicate nor counts as `absent:true` evidence — polling simply
continues until the budget runs out.
`screenshot` captures PNG natively; `format:"jpeg"` and/or `max_dim`
re-encode/downscale on the daemon host before the bytes cross the wire
(a full-resolution simulator PNG of several MB typically compresses to
tens of KB at `format:"jpeg", max_dim:800`). Omitting the new fields
preserves the original PNG behaviour exactly.

When the journal is enabled, a successful `screenshot` is also stored as a
content-addressed run artifact and referenced from the action's journal
entry; `inline:false` strips the base64 payload from the response only, so
the capture can still be fetched from the journal. On a journal-less daemon
`inline:false` still omits the bytes (nothing retains them).

`audit` runs deterministic geometry checks over one a11y-tree observation
and produces findings — measured evidence with an `evidence` sentence per
finding, never pass/fail verdicts. Each finding's `ref` (`F1`, `F2`, ...)
matches a labeled red box on the returned annotated screenshot. Dense
grids of repeated same-role, same-size sibling controls (keyboards, emoji
grids, calendar day cells) are suppressed. When the journal is enabled the
findings JSON (`audit-findings.json`) and the annotated PNG
(`audit-annotated.png`) are both stored as content-addressed run artifacts
on the action's entry; `inline:false` strips only the wire payload, like
`screenshot`.

Waiting (deterministic sync primitives; poll the same a11y pipeline as
`observe`):

| `kind` | payload | result |
|---|---|---|
| `wait_for_element` | matcher (`label?`, `role?`, `value?`, `id?`, `placeholder?`, `exact?`, `match?`, `in_frame?` — or `predicate?`, see the element actions), `absent?`, `refresh?`, `timeout_ms?` (default 10000, max 120000), `interval_ms?` (default 500, min 10) | `element` (matched node incl. `frame`, no children) or `absent:true`, `elapsed_ms`, `polls`, `hash` (tree hash of the final poll) |
| `wait_tree_stable` | `stable_samples?` (default 3, 2–20), `timeout_ms?` (default 15000, max 120000, doubles as the max wait), `interval_ms?` (default 500, min 10), `require_stable?` (default false) | `stable`, `hash`, `settled_ms`, `samples` |

`wait_for_element` polls a fresh compacted tree each interval until an
element matching the predicate appears (or, with `absent:true`, until no
element matches). At least one predicate field is required. `role` and
`id` always match exactly; `label`, `value`, and `placeholder` match by
substring unless `exact:true` (or the equivalent `match: "exact"`;
`match: "substring"` is the explicit default). `placeholder` matches the
node's placeholder or its value, because an empty text field commonly
surfaces its placeholder as the AX value (React Native `TextInput`s in
particular expose empty labels and placeholder-as-value). `in_frame`
(`{x,y,w,h}`) requires the element's frame centre to lie inside the
rectangle. When several elements match, the best-ranked one wins: exact
text matches beat substring hits, interactable elements (buttons, links,
text fields, …) beat static text and containers, and the smallest frame
wins among the rest — so a `Group` whose label concatenates its children,
or description copy containing the requested word, loses to the actual
control. On success the matched element is returned with its `frame` so
the caller can tap it directly. `refresh: true` makes each poll bypass
the resident warm helper (see `observe`).

`wait_tree_stable` polls until the tree hash (the same digest `observe`
returns) is identical for `stable_samples` consecutive polls — i.e.
animations and loads have settled — and returns `stable:true` with the
final `hash` plus the settle duration.

`timeout_ms` is a **max wait**, not a deadline that fails: a screen that
never holds still (a looping React Native onboarding animation, a
spinner, a video) returns `200` with `stable:false`, the last observed
`hash`, and the polls/elapsed spent, so callers can tell "the UI is
genuinely live" apart from a broken poll. Only real poll failures (AXe
missing, target not booted, backend error, or a tree that was never
readable at all — every poll failed, so the last hash is empty) are
errors. On animated
screens prefer `wait_for_element` (or `tap_element` /
`type_into_element`, which embed the same wait) — waiting for the
element you actually need is deterministic there, while tree stability
never arrives. Callers that genuinely need a settled tree (e.g. an eval
capturing a reproducible tree hash) set `require_stable:true` and get the
old `408 timeout` behaviour instead.

`wait_for_element` returns `408 {"code":"timeout"}` when its budget is
exhausted; other poll failures map to the usual action errors.

Pasteboard (simctl):

| `kind` | payload | result |
|---|---|---|
| `pasteboard_set` | `text` | `copied_runes` |
| `pasteboard_get` | — | `text` |

App lifecycle (simctl):

| `kind` | payload | result |
|---|---|---|
| `install_app` | `path` (a `.app` on the daemon host) | `installed` |
| `launch_app` | `bundle_id`, `terminate_running?`, `args?` | `bundle_id`, `pid` (number) |
| `terminate_app` | `bundle_id` | `terminated` |

The same actions are available over the WS surface as
`actions.dispatch` with the identical params/result bodies.

### 5.2 Physical devices (kind `device`)

Daemons started with `--devices` also enumerate paired physical devices
via `devicectl` as `Target`s with `kind:"device"` (see
[docs/devices.md](../docs/devices.md) for setup). Device semantics differ
from simulators in these protocol-visible ways:

- **Enumeration/state**: `state` maps from the CoreDevice tunnel —
  connected → `Booted`, otherwise `Unknown` plus a `disconnected` label.
  Labels are derived as for simulators but start with `device` instead of
  `simulator`.
- **Boot/shutdown**: `POST /v0/targets/{udid}/boot` and `/shutdown` on a
  device return `501 {"code":"not_implemented"}` — a phone cannot be
  booted or shut down remotely.
- **Leases**: acquiring with a `reset` spec (`erase` / `snapshot:<name>`)
  that names a device (pinned `udid`, a `device` label, or a label set
  whose only matches are devices) is `400 {"code":"bad_request"}` — there
  is no `simctl erase` equivalent for devices. A reset-carrying request
  never matches a device: when only devices match, it is
  `409 {"code":"no_match"}` rather than queueing. Leases without a reset
  spec grant normally.
- **Streams**: `stream.open` (and the HTTP stream endpoints) on a device
  return `501 {"code":"not_implemented"}` — streaming is simulator-only.
- **State/images**: the §7 state engine and golden images are
  simulator-only; device targets are never snapshotted or reset. State
  endpoints (`/v0/state/*` and their WS methods) on a lease holding a
  device return `501 {"code":"not_implemented"}`.
- **Actions**: only the kinds below are accepted. Recognized
  simulator-only kinds (`key`, `key_sequence`, `audit`, and any other kind the
  simulator backend implements but WDA cannot express) are
  `501 {"code":"not_implemented"}`; unknown kinds stay
  `400 bad_request`. HID/observation kinds require a WebDriverAgent
  endpoint configured for the device (`--device-wda <udid>=<url>`),
  otherwise `503 {"code":"unavailable"}`. Lifecycle kinds run through
  `devicectl`; a paired-but-unreachable device is
  `503 {"code":"unavailable"}` with a "device not connected" message.

| `kind` | payload | result |
|---|---|---|
| `install_app` | `path` (a `.app` on the daemon host) | `installed` |
| `launch_app` | `bundle_id`, `terminate_running?`, `args?` | `bundle_id` (no `pid`: devicectl does not report one) |
| `terminate_app` | `pid` (positive integer; devicectl terminates by process id, not bundle id) | `terminated_pid` |
| `tap` | `x`, `y` (>= 0) | `x`, `y`, `backend:"wda"` |
| `swipe` | `start_x`, `start_y`, `end_x`, `end_y`, `duration_seconds?` | echo of the coordinates, `backend:"wda"` |
| `type` | `text` | `typed_runes`, `backend:"wda"` |
| `button` | `name` (`home`\|`lock`\|`volume-up`\|`volume-down`) | `button`, `backend:"wda"` |
| `pasteboard_set` | `text` | `copied_runes`, `backend:"wda"` |
| `pasteboard_get` | — | `text`, `backend:"wda"` (WDA requires its app foregrounded on iOS 13+) |
| `observe` | `include_raw?`, `format?`, `interactive_only?`, `roles?`, `scope?`, `exclude_system_chrome?` (same semantics as the simulator observe) | `tree` (compacted a11y nodes mapped from the XCUITest source; `tree_compact` when `format:"compact"`), `hash`, `total_elements?`/`returned_elements?`, `raw?` (XCUITest XML when `include_raw`), `raw_format?:"xcuitest-xml"`, `backend:"wda"` |
| `tap_element` | same fields as the simulator kind | same result shape |
| `type_into_element` | same fields as the simulator kind | same result shape |
| `wait_for_element` | same fields as the simulator kind | same result shape |
| `wait_tree_stable` | same fields as the simulator kind | same result shape |
| `scroll_to_element` | same fields as the simulator kind | same result shape (swipes via WDA) |
| `screenshot` | same fields as the simulator kind | same result shape, `backend:"wda"` |

> Device `observe` changed in this version: it previously returned the raw
> XML as `source`/`format`; it now returns the same compacted `tree` +
> `hash` shape as simulators (the raw XML is available via
> `include_raw`). This is the one non-additive device-result change in
> v0, made before any external consumer of the device surface existed so
> element predicates work identically across target kinds.

## 6. Streams (owned by the stream slice)

Viewing is **read-only and requires no lease**. See
[docs/streaming.md](../docs/streaming.md) for the implementation.

- `POST /v0/streams` body `StreamRequest` (`udid` or `lease_id` to identify
  the target, `format?` — `mjpeg` (default; `h264` is reserved and rejected
  with `bad_request` in v0.1), `max_fps?`, `max_dim?` — longer-edge pixel
  cap, frames are downscaled server-side — and `quality?` — JPEG re-encode
  quality 1–100) → `200 {StreamOffer}`:
  `stream_id`, `format`, `url` (WS endpoint, one binary JPEG per message),
  `mjpeg_url` (HTTP `multipart/x-mixed-replace`), `view_url` (browser view
  page), `fps` — the effective capture rate after clamping the requested
  `max_fps` to `--stream-max-fps` — `max_dim`/`quality` echoing the
  effective frame transform, if any — and `holder` — the target's current
  active lease, if any; its `id` (a capability token) is redacted unless
  the request presented that same `lease_id`. The target must be `Booted`; otherwise
  `409 {"code":"target_busy"}`. Opening is idempotent per target: all
  viewers share one stream, so the first request's `max_dim`/`quality`
  win for the stream's lifetime (individual viewers can shrink further
  with the attach-time query params below). When the daemon is at
  `--stream-max-streams` capacity, `429 {"code":"stream_limit"}`.
- `GET /v0/streams/{id}/mjpeg?max_dim=&quality=` — attach as an MJPEG
  viewer (browser `<img>`, curl). The optional `max_dim` (1–4096) /
  `quality` (1–100) query params downscale/re-encode frames for this
  viewer only, applied on top of any stream-level transform. Attach errors carry the §1 JSON
  envelope: `404 not_found` for an unknown/reaped stream id,
  `429 viewer_limit` past `--stream-max-viewers`, `410 stream_gone` for a
  stream torn down mid-attach; out-of-range query params are
  `400 bad_request`.
- `GET /v0/streams/{id}/ws?max_dim=&quality=` — attach as a WS viewer
  (binary JPEG frames). Same query params and attach errors as `/mjpeg`.
- `DELETE /v0/streams/{id}` → `200 {"ok":true}` — tear the stream down,
  disconnecting all viewers.
- `GET /view/{udid}` — static browser view page for the target.
- WS method `streams.open` mirrors `POST /v0/streams`.

Capture starts on the first viewer attach and stops `--stream-linger`
(default 10s) after the last viewer detaches. A viewer that stops reading
entirely (no frame accepted for 30s) is disconnected so it does not pin a
viewer slot.

## 7. State engine (owned by the state slice)

Deterministic environment control: snapshots of shutdown sims, fixtures,
and per-lease auto-reset. See `docs/state.md` for semantics and payload
schemas. Every mutating op requires an active lease; the server derives the
target UDID from the lease, so a client can only mutate its own target.
On hosts without simctl these endpoints return `501 not_implemented`.

- `POST /v0/state/snapshots` body `SnapshotRequest` (`lease_id`, `label?`)
  → `201 {SnapshotInfo}`. Requires the target to be shutdown, else
  `409 {"code":"target_busy"}`.
- `GET /v0/state/snapshots?lease_id=<id>` → `{"snapshots": [...]}`.
  Requires an active lease; only snapshots taken from the lease's target
  are returned.
- `DELETE /v0/state/snapshots/{id}?lease_id=<id>` → `200 {"ok":true}`.
  Requires an active lease; the snapshot must have been taken from the
  lease's target, else `404 not_found`.
- `POST /v0/state/restore` body `RestoreRequest` (`lease_id`, `snapshot`
  — ID or label, `reboot?`) → `200 {RestoreResult}` with `rebooted` true
  when the engine performed the explicit shutdown+restore+boot cycle for a
  booted target. A booted target without `reboot:true` →
  `409 {"code":"target_busy"}`.
- `POST /v0/state/erase` body `EraseRequest` (`lease_id`) →
  `200 {"ok":true}`; booted target → `409 {"code":"target_busy"}`.
  On a sim stamped from a slim image, a successful erase is followed by a
  re-apply + verify of the image's launchctl disable set (boot → disable
  → verify → shutdown), so the call takes correspondingly longer and
  fails (`internal`) when the sim's slim state could not be restored —
  the disk wipe itself has still happened.
- `POST /v0/state/fixtures` body `FixtureRequest` (`lease_id`, `name`,
  `payload`) → `200 {"ok":true}`. Fixture names: `statusbar`, `privacy`,
  `push`, `locale`, `timezone`, `url`.

WS methods mirror REST: `state.snapshot`,
`state.snapshots.list` (params `{"lease_id": ...}`),
`state.snapshots.delete` (params `{"lease_id": ..., "id": ...}`),
`state.restore`, `state.erase`, `state.fixture`.

Per-lease auto-reset: `AcquireLeaseRequest.reset` may be `"none"`
(default), `"erase"`, or `"snapshot:<name>"`. On builds without a state
engine, non-`none` specs are rejected with `not_implemented` (a reset that
could never run must not be silently accepted). If a `snapshot:<name>` spec
cannot be resolved when the reset runs (typo, or the snapshot was deleted
mid-lease), the reset degrades to an `erase` — the next holder still gets a
clean target — instead of quarantining the target. When the lease ends (release
or expiry), the daemon holds the target, applies the reset (leaving the sim
shutdown), and only then hands it to the next holder. If the reset fails
the target is quarantined (never handed out dirty) and queued leases stay
queued until an operator clears it with
`POST /v0/targets/{udid}/clear-quarantine` → `200 {"ok":true}` (a no-op if
the target is not quarantined; `409 target_busy` while the reset is still
running). Resets are bounded by a 10-minute timeout, so a hung simctl
becomes a quarantine rather than holding the target forever. For sims
stamped from a slim image, the erase reset includes the post-erase
re-apply/verify cycle; that recovery step survives client disconnects
and is guaranteed a small floor budget (3 minutes) even when the erase
consumed most of the reset window, so a reset can in the worst case run
slightly past the 10-minute bound before quarantining. A sim whose slim
state cannot be restored fails the reset and is quarantined rather than
returned to the pool un-slimmed.

Erase-on-grant: a `reset:"erase"` acquire is additionally guaranteed a
clean target at grant time. If the matched target was left dirty by a
previous holder whose own reset never ran (e.g. that lease carried
`reset:"none"`), the grant is deferred — the request is answered
`202 {"state":"queued"}` — while the daemon erases the target, and only
promoted to `active` once the erase succeeds. A failed pre-grant erase
quarantines the target like any failed reset. Clean free targets are
preferred over dirty ones for erase-carrying requests, and targets
already cleaned by a completed post-lease reset (or a quarantine-recovery
rebuild, which always erases) are granted immediately without a
redundant second erase. At most one pre-grant erase runs per queued
lease at a time.

### 7.1 Golden images

Build-once/stamp-many simulator provisioning (see `docs/images.md`).
Images are fleet-level resources — no lease guards these routes, because
build/stamp/delete only ever operate on simulators the image flow itself
creates. On hosts without simctl they return `501 not_implemented`.

- `POST /v0/images/build` body `ImageBuildRequest` (`device_type`,
  `runtime`, `name?`, `slim_profile?`) → `201 {ImageInfo}`. `runtime`
  takes a display name (`"iOS 26.5"`, resolved via `simctl list
  runtimes`) or a CoreSimulator identifier; an unknown runtime or device
  type → `400 {"code":"bad_request"}`. With a
  `slim_profile` on a host without simslim → `503 {"code":"unavailable"}`;
  with a `slim_profile` on an iOS runtime older than 18 →
  `400 {"code":"bad_request"}` (simslim silently no-ops there). Slim
  builds record the captured launchctl disable set and measured post-slim
  footprint in `ImageInfo` (`disabled_services`, `disabled_count`,
  `post_slim_procs`); stamping re-applies and verifies the disables on
  every created sim (see `docs/images.md`).
- `GET /v0/images` → `{"images": [...]}`.
- `POST /v0/images/{id}/stamp` (image ID or name) body
  `ImageStampRequest` (`count` 1..16, `name_prefix?` default `manzanas`)
  → `201 {ImageStampResult}` (`created` `[{udid,name}]`, `duration_ms`).
  Stamped sims appear in `/v0/targets` as ordinary leasable targets.
  A sim booted out-of-band mid-stamp → `409 {"code":"target_busy"}` with
  full rollback; an archive that fails its recorded SHA-256 →
  `409 {"code":"unavailable"}` (rebuild or delete the image).
- `DELETE /v0/images/{id}` → `200 {"ok":true}`. Already-stamped sims are
  unaffected.

WS methods mirror REST: `images.build`, `images.list`,
`images.stamp` (params `{"id", "count", "name_prefix"}`),
`images.delete` (params `{"id"}`). Because these can run for minutes,
they are dispatched off the connection's read loop (other requests keep
flowing); at most 4 may be in flight per connection — beyond that the
request is rejected with `{"code":"overloaded"}` (back off and retry).

## 8. Journal (owned by the journal slice)

Journal entries are referenced from `ActionResult.journal_ref` as
`{"run_id","seq"}`. A run groups entries per lease; the run ID **is** the
lease ID. Entry format, on-disk layout, and versioning rules live in
[`docs/journal.md`](../docs/journal.md). All endpoints return `501` when
the daemon runs with the journal disabled (`--journal-dir ""`). Reading a
run whose `meta.json` declares a `format_version` this daemon does not
implement also returns `501 not_implemented` (readers refuse formats they
don't know).

Every mutating protocol op under a lease is journaled: lease lifecycle
(including expiry, as `leases.expire`), target boot/shutdown, actions,
artifact ingest, the mutating state ops (`state.snapshot`, `state.restore`,
`state.erase`, `state.fixture`, `state.snapshots.delete` — kind `state`),
and the post-lease auto-reset (`state.reset`).

- `GET /v0/journal` → `200 {"runs":[RunSummary, ...]}` — newest first.
- `GET /v0/journal/{run}?from_seq=&limit=` →
  `200 {"run_id","meta","entries":[Entry,...],"next_seq"}`.
  `limit` defaults to 100 (max 1000); `next_seq` is 0 when the page reached
  the end, else pass it back as `from_seq` for the next page.
  Unknown run → `404 {"code":"not_found"}`.
- `GET /v0/journal/{run}/artifacts/{path}` → artifact bytes with a
  best-effort `Content-Type` (paths come from entry `artifacts[].path`,
  relative to the run dir, e.g. `artifacts/<sha256>.png`).
- `POST /v0/journal/{run}/artifacts?name=&kind=` with the raw bytes as the
  body → `201 {"artifact":{"path","sha256","bytes"},"journal_ref"}`.
  Stores the artifact content-addressed and appends an entry referencing
  it. `kind` must be one of `observation` (default), `screenshot`, or
  `video`; other values → `400 bad_request`. The entry is journaled under
  the `journal/v0` entry kinds (`observation` for screenshots, `segment`
  for video), with the requested kind preserved in the entry params as
  `artifact_kind`. The run's lease must still be
  active — once it is released or expires the journal is immutable and
  uploads return `409 lease_expired`. Used by action backends and clients
  uploading evidence (e.g. screenshots).
- `GET /v0/journal/{run}/export.md` → `200` `text/markdown` — a
  PR-comment-ready evidence summary (run metadata, action table, artifact
  list).
- WS `journal.tail` params `{"run_id","from_seq"}` → result acks the
  subscription, then entries from `from_seq` (replay) and all future
  entries stream as `journal.entry` events on the same connection.
  Subscriptions last for the connection's lifetime; one per run, at most
  16 per connection (duplicates or excess → `400 bad_request`).
