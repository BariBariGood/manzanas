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
  `not_implemented`, `bad_request`, `lease_expired`, `lease_paused`
  (`409` — the lease is paused for human preemption; only read-only
  observation is dispatchable until it is resumed), `no_match`,
  `target_busy`, `target_not_booted`, `stream_limit`, `viewer_limit`,
  `stream_gone`, `timeout`, `off_viewport`, `ambiguous_match` (`409` — a
  structured predicate matched several elements and no `index` picked
  one; the message lists the candidates), `focus_required` (`412` — a
  typing action with `require_focus:true` found no focused text field),
  `assertion_failed` (`417` — an `assert_*` action's postcondition was
  observed to be false within its retry budget),
  `unauthorized` (`401` — the daemon/broker runs with `--auth-token` and
  the request carried a missing or wrong bearer token),
  `internal`. Two additive
  optional fields: `detail` carries actionable context when the daemon
  has any (e.g. a `target_not_booted` error against a leased target
  reports who shut the target down and why, or that the daemon has no
  record of doing so), and `retry_after_seconds` is a machine-readable
  retry hint on transient refusals (`overloaded`), mirrored in an HTTP
  `Retry-After` header.
- Timestamps are RFC 3339 UTC.
- `GET /v0/healthz` returns `200 {"ok":true,"version":"v0","build":"v0.4.0"}`.
  `version` is the protocol version; `build` (additive) is the binary's
  build version stamped at link time (`git describe` / release tag;
  `"dev"` for unstamped builds). The broker's healthz additionally
  carries `role` and `hosts`.
- Authentication (optional): daemons and brokers started with
  `--auth-token <t>` (env `MANZANASD_AUTH_TOKEN` /
  `MANZANAS_BROKER_AUTH_TOKEN`) require the shared token on every
  endpoint except `GET /v0/healthz`, CORS preflights, and the dashboard /
  view-page static assets. Clients send `Authorization: Bearer <t>`;
  browser contexts that cannot set headers (the MJPEG `<img>`, the
  events WebSocket, view links) may use a `?token=<t>` query parameter
  instead. Failures are `401 unauthorized`. With no `--auth-token`
  (the default) nothing changes: the open, tailnet-trust model. The
  broker authenticates to fronted daemons with per-host tokens (config
  `token` per host, or the fleet-wide `--daemon-token` default).
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
  `load_avg1`, `cpus`, `free_disk_bytes`, `build` (optional/additive:
  the daemon's build version), and `gates`
  (`load_ok`/`disk_ok` — a false gate means a cold boot would be refused
  right now). When advice has been received (below), the snapshot also
  carries `pool_advice` (`PoolAdviceState`: `source`, `received_at`,
  `window_seconds`, `classes`). Additive: schedulers must tolerate older
  daemons answering `404`.
- `GET /v0/doctor` returns the daemon's setup diagnosis for
  `manzanas doctor` (`DoctorReport`): `ok` (false iff any check failed),
  `build`, `protocol`, and `checks` — each a `DoctorCheck` with `name`,
  `status` (`ok`|`warn`|`fail`|`n/a`), `detail`, and — for warn/fail — a
  one-line actionable `remedy`. Checks cover the daemon itself, the
  simulator toolchain (`xcrun`, `ios_runtime`, `devicectl`), the run
  `journal` dir, the warm pool's `boot_gates`, and per configured
  physical device `wda:<udid>` reachability and `mirror:<udid>`
  (plus `mirror:<udid>:<probe>`) — the mirrord helper's `/v1/doctor`
  capability probes (§5.3), whose detail names the responsible process
  for a missing TCC grant. `n/a` marks checks that do not apply to the
  host (simulator tooling on a `--mock`/non-macOS daemon, or device
  probes when the devices config has `enabled:false` — the daemon runs
  no WDA/mirror clients then, so live probes are skipped) — never a
  failure. `?target=<udid>` scopes the per-device checks to one UDID
  (an unmatched target adds a failing `target` check, including when no
  devices are configured or they are disabled). Failed checks
  are data: the route still answers `200`. Additive: older daemons
  answer `404` (builds without a doctor wired answer
  `501 not_implemented`), which clients must tolerate.
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

A mirror-backed physical device (§5.3) additionally carries a `mirror`
label, with its `state` derived from the mirrord helper: `ready` →
`Booted`; helper not running / no phone window / socket unreachable →
`Unknown` plus `disconnected`. A blocked mirroring session stays
`Booted` plus a `mirror-blocked` label; because the registry's routine
liveness probe is the light status (§5.3), which cannot see the
interstitial, the label is derived from a periodic full (OCR-backed)
status and can lag the interstitial by up to about a minute — the block
itself surfaces immediately on the action/observe paths. A mirror device whose
CoreDevice tunnel is down keeps working through the mirror but carries a
`no-tunnel` label (devicectl-routed `install_app`/`launch_app`/
`terminate_app` fail); one that devicectl does not enumerate at all is
synthesized from the mirror config with a `no-devicectl` label (same
actions fail). See [docs/devices.md](../docs/devices.md).

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
- `POST /v0/leases/{id}/pause` body `PauseLeaseRequest`
  (`reason?` — freeform, surfaced as `paused_reason`) → `200 {Lease}` with
  the additive fields `paused: true`, `paused_at`, and `paused_reason?`
  stamped on the (still `active`) lease. Human preemption: while paused,
  dispatching actions on the lease is refused with
  `409 {"code":"lease_paused"}` — except the read-only kinds `observe`
  and `screenshot`, which stay available so the holder can watch the
  human's progress. Pause does **not** stop the TTL clock: the holder
  must keep renewing (the grace window applies as usual), so an
  abandoned pause can never hold a target forever. Pausing an
  already-paused lease keeps the original `paused_at` and only updates
  `paused_reason` when a non-empty reason is given. Only active leases
  can pause (terminal → `410 lease_expired`, unknown → `404`). Fires a
  `lease.paused` WS event. v1 pause is explicit and API-driven (an agent
  or human tooling calls it); automatic human-presence detection from
  mirrord is future work.
- `POST /v0/leases/{id}/resume` → `200 {Lease}` with the pause fields
  cleared, unblocking action dispatch. Resuming a non-paused active
  lease is a no-op success; terminal → `410`, unknown → `404`. Fires a
  `lease.resumed` WS event. A lease that expires or is released while
  paused ends normally (the pause fields are cleared on the terminal
  transition).
- `DELETE /v0/leases/{id}` → `200 {Lease}` with `state:"released"`.
  Releasing (or expiry) hands the target to the head of the matching queue.
  Release is idempotent: for a lease already in a terminal state
  (`expired` or `released`) it is a no-op and returns the lease with its
  current terminal state unchanged.

A `Lease` served by a federating broker additionally carries `host` and
`host_addr` (the owning daemon's base URL): clients should perform all
target-bound ops (boot, actions, streams, state, recording, journal)
against `host_addr` directly. The bundled `manzanas` CLI and MCP facade
do this automatically (their shared client caches `lease_id → host_addr`
and routes lease-scoped calls to the owning daemon); custom raw-HTTP
clients must follow `host_addr` themselves. See
[docs/broker.md](../docs/broker.md).

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
`leases.acquire`, `leases.get`, `leases.renew`, `leases.pause` (params
`{id, reason?}`), `leases.resume` (params `{id}`), `leases.release`,
`actions.dispatch`, `actions.batch`, `streams.open`. Params/results are the
same JSON bodies as REST. Events: `lease.granted`, `lease.expiring`
(an active lease entered its renewal grace window — renew now or it
expires for real at `grace_until`), `lease.expired`, `lease.paused`,
`lease.resumed` (human preemption, §3), `target.state`.

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
  `410 lease_expired`. A paused lease (§3, human preemption) refuses
  everything except the read-only `observe`/`screenshot` kinds with
  `409 lease_paused`. Terminal (released/expired) leases stay resolvable
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
| `tap` | `x`, `y` (>= 0), `duration_ms?` (number, 1–10000: hold the press that long — a long-press; out-of-range is `400 bad_request`) | `x`, `y`, `duration_ms?` (echoed when a long-press) |
| `swipe` | `start_x`, `start_y`, `end_x`, `end_y` (>= 0), `duration_seconds?` | echoes the path |
| `type` | `text` (chars AXe can map to HID keycodes; an unmappable char such as `\t` is `400 bad_request`, and any prefix before it may already have been typed), `strategy?` (`hid` default \| `paste`), `require_focus?` (default `false`) | `typed_runes`, `strategy?` (echoed when `paste`) |
| `button` | `name` ∈ `home`, `lock`, `side-button`, `siri`, `apple-pay` | `button` |
| `key` | `keycode` (HID usage code; integer in `[0, 2^32-1]`, else `400 bad_request`), `duration_seconds?` | `keycode` |
| `key_sequence` | `keycodes` (array; each an integer in `[0, 2^32-1]`, else `400 bad_request`) | `count` |
| `tap_element` | matcher (`label?`, `role?`, `value?`, `id?`, `placeholder?`, `exact?`, `match?`, `in_frame?` — or `predicate?`, see below), `anchor?` (`start` \| `center` (default) \| `end`), `refresh?`, `timeout_ms?` (default 10000, max 120000), `interval_ms?` (default 500, min 10) | `element` (matched node, no children), `x`, `y` (tapped point), `elapsed_ms`, `polls` |
| `type_into_element` | same matcher/timing/anchor fields plus `text`, `strategy?` (`hid` default \| `paste`), `require_focus?` (default `false`) | `tap_element`'s fields plus `typed_runes`, `strategy?` (echoed when `paste`) |
| `scroll_to_element` | matcher (`label?`, `role?`, `value?`, `id?`, `placeholder?`, `exact?`, `match?`, `in_frame?` — or `predicate?`), `anchor?`, `direction?` (`down` (default) \| `up` \| `left` \| `right`), `max_scrolls?` (default 8, max 30), `refresh?`, `timeout_ms?` (default 30000, max 120000), `interval_ms?` (default 500, min 10) | `element` (matched node incl. `frame`, no children), `scrolls`, `polls`, `elapsed_ms`, `hash` (tree hash at success) |
| `scroll_until` | `text` (non-empty; v1 text-only — case-insensitive substring of a visible node's label, value, or placeholder), `direction?` (`down` (default) \| `up` \| `left` \| `right`), `max_scrolls?` (default 8, max 30), `moved_jaccard?` (default 0.6, in `(0, 1]`), `refresh?`, `timeout_ms?` (default 30000, max 120000), `interval_ms?` (default 500, min 10) | `found`, `element?` (matched node, when found), `scrolls`, `screens`, `moved_history`, `end_of_list`, `elapsed_ms`, `hash` |
| `scroll_collect` | `rect?` (object with numeric `x`, `y`, `w`, `h`; harvest only boxes whose centre falls inside it), `direction?`, `max_scrolls?`, `moved_jaccard?`, `refresh?`, `timeout_ms?`, `interval_ms?` (same defaults as `scroll_until`) | `screens_texts` (per-screen deduped text boxes), `collected`, `scrolls`, `screens`, `moved_history`, `end_of_list`, `elapsed_ms`, `hash` |
| `tap_template` | `template_base64` (inline base64 PNG/JPEG template) or `name` (+ `bundle_id?`, default `_global`) — a stored template saved by `save_template`, looked up under the capture's device class; exactly one of the two — `threshold?` (NCC score in (0, 1], default 0.8), `rect?` (`{x,y,w,h}` search region, pixels in the capture image), `timeout_ms?` (default 3000, max 120000), `interval_ms?` (default 500, min 10) | `matched:true`, `x`, `y` (the tapped match centre, tap coordinates), `score`, `threshold`, `elapsed_ms`, `polls`, `name`/`bundle_id` (echoed for named lookups) |

`tap_template` is the matching half of the template library (see §5.1
`save_template` and [docs/templates.md](../docs/templates.md)): each poll
takes a **fresh** capture through the backend's normal screenshot path,
grayscales it and the template, matches by normalized cross-correlation
(`internal/imgutil.MatchNCC`), and — once the best score reaches
`threshold` — taps the centre of the matched region. `rect` restricts
the search to a sub-region of the capture (a rect that does not fit is
`400 bad_request` naming the capture size); match positions stay in
full-capture pixel coordinates either way, and the tap converts them to
the backend's tap space (points on simulators and WDA devices via the
observed viewport width, capture pixels as-is on the mirror). Named
lookups resolve like `assert_template`: a daemon without
`--templates-dir` is `503 unavailable`, and a name the store lacks for
the capture's device class is `404 not_found`. A budget that expires
without a match is `408 timeout` whose message carries the best score
seen, so callers know how close the screen came. It is a mutating kind:
on mirror-backed devices it is gated by `allow_input` like `tap`
(§5.3).

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

`tap_element` and `type_into_element` resolve the element through a
backend-registered **resolver ladder** (additive, v0; see
docs/resolver-ladder.md): ordered resolution rungs — the element-map
fast path (`map`, first, when the daemon has an element map — see §5.5
and docs/elementmap.md), the a11y tree (`ax`), then OCR text match
(`ocr`) where an OCR source exists (mirror-backed devices; WDA-only
devices register no OCR rung), then a saved-template match
(`template`, on daemons with a template library): when an earlier rung
misses, a template whose name matches the flat matcher's `id` or
`label` (as-is or slugged, `"Sign In"` → `"sign-in"`; structured
`predicate` matchers never key a template) is looked up under the
launched bundle then `_global` for the capture's device class and
NCC-matched against a fresh capture at the default 0.8 threshold —
each attempt bounded by a per-rung budget. Successful results
additionally carry `resolver` (the rung name that resolved the
element: `"map"`, `"ax"`, `"ocr"`, `"template"`, ...), and
`resolver_skipped` (e.g.
`"ax(circuit-open)"`) when a rung was skipped by the per-lease,
per-bundle AX circuit breaker. When no rung resolves the element within
`timeout_ms` the `408 timeout` is an **escalation**: alongside the
off-screen hint, the journal entry's `escalation` field carries a
compact observation bundle — the per-rung outcome trail (`rungs`), the
last observation's text boxes (`text_boxes`, up to 24
`{text,x,y,w,h}`), any `resolver_skipped` — plus a best-effort
`escalation.png` screen capture artifact. Journal v0 readers ignore
these additive keys.

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

`scroll_until` and `scroll_collect` are list-scrolling composites: one
call replaces an agent round-trip per screen. Each iteration gestures
once (`direction` is the content edge to reveal, as in
`scroll_to_element`), waits for the screen to settle (bounded poll until
the masked text set holds still), then decides whether the list actually
moved by comparing the before/after text sets — masked against the
volatile status-bar/home-indicator bands so a ticking clock does not
read as movement — with Jaccard overlap: overlap `>= moved_jaccard`
(default 0.6) is a non-move, so a bounce-back overscroll at the end of a
list is not mistaken for progress. End-of-list is two consecutive
non-moves, each confirmed by a lazy-load grace re-poll (a list paused on
a loading spinner gets one more beat to resolve into new rows before the
non-move counts). On simulators and WDA devices the gesture is a swipe
at the standard 40% fraction; on mirror-backed devices it is a trackpad
wheel scroll (falling back to a momentum flick when the wheel path is
unavailable, e.g. an older mirrord without `/v1/scroll`; horizontal
directions always flick since wheel events carry no dx). Mirror
per-action verification cooperates with end-of-list detection rather
than fighting it: a gesture the helper delivered and pixel-verified to
have changed nothing (`input-not-delivered`, or an unverified ack whose
check ran) is not fatal — by construction the detector needs a
no-change gesture to succeed. The verdict is not trusted blindly
either (the helper can miss a late render), so the loop still settles
and re-observes, letting the ordinary Jaccard moved check decide the
non-move; `verify:"unavailable"` (nobody could look, the screen may
have moved) remains a hard `503 unavailable`. The same rule applies to
`scroll_to_element`'s swipes: two consecutive verified no-change
swipes end in its documented `timeout`/`off_viewport` failure — after
one more confirming observation — instead of an input error.

`scroll_until` stops as soon as `text` matches a visible node
(case-insensitive substring of label, value, or placeholder — v1 is
text-only, so it works identically over OCR trees). Not finding the text
is a **result**, not an error: `{found: false}` with `end_of_list`
telling the caller whether the list ran out of content or `max_scrolls`
/ `timeout_ms` ran out first. `moved_history` carries the per-gesture
Jaccard overlaps (one entry per gesture) so a caller can see how the
list behaved; `screens` counts settled observations examined.

`scroll_collect` harvests every visible text run as a `{text, x, y, w,
h}` box while scrolling to the end of the list (or `max_scrolls`),
deduplicating across screens by normalized text — a sticky header
appears once, on the first screen it was seen. `screens_texts` is the
per-screen list of newly-seen boxes (`index`, `texts`); `collected` is
the total. An optional `rect` restricts harvesting to boxes whose
centre falls inside it.

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
| `save_template` | `name` (template name; letters, digits, `.`, `-`, `_`), `rect` (`{x,y,w,h}` pixels in the capture image), `bundle_id?` (grouping key, default `_global`), `source_run?` (run/lease id recorded in the sidecar) | `name`, `bundle_id`, `device_class` (`"<backend>/<img_w>"`), `window_px` (`[w,h]` of the full capture), `rect`, `bytes`, `sha256`, `backend` |
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

`save_template` takes a **fresh** capture through the backend's normal
screenshot path, crops `rect` out of it, and stores the crop in the
daemon's template library (see [docs/templates.md](../docs/templates.md)
and §5.4) as `<bundle_id>/<name>@<device_class>` with a JSON metadata
sidecar. It is the storage half of pixel-template matching; the
matching half is `tap_template`, `assert_template`, and the resolver
ladder's `template` rung. A `rect` that does not fit inside the
capture is `400 bad_request` (the error names the capture size); a
daemon started without a template library (`--templates-dir ""`) answers
`503 unavailable`. The action journals normally.

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
| `wait_for_stable` | `quiet_ms?` (default 500), `timeout_ms?` (default 15000, max 120000, doubles as the max wait), `interval_ms?` (default 500, min 10), `threshold?` (default 0.002, exclusive 0..1), `mask?` (list of `{x,y,w,h}` capture-pixel rects excluded from the diff), `require_stable?` (default false) | `stable`, `waited_ms`, `last_diff`, `samples` |

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

`wait_for_stable` is the pixel-diff counterpart: instead of the a11y
tree it polls **screenshots**, downscales each capture to quarter-
resolution grayscale, and compares consecutive frames with a normalized
mean-absolute-diff. The screen counts as settled once every
frame-to-frame diff stays under `threshold` for the `quiet_ms` window.
`mask` rectangles (capture-image pixels) are excluded from the diff so
volatile regions — clocks, spinners, blinking cursors — don't keep the
wait alive. Because it needs no accessibility tree or OCR pass per
poll, it works identically on every backend and is the cheap settle
primitive on mirror-backed devices. `last_diff` is the final observed
diff; `samples` counts captures taken. A wait whose budget expires
before two frames were ever captured (every poll failed — a dead WDA
runner, a mirroring hiccup on every capture) is an error, not a soft
result, mirroring wait_tree_stable's never-readable-tree rule.

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

Verification (assertion kinds plus opt-in verified taps):

| `kind` | payload | result |
|---|---|---|
| `assert_text` | `text`, `fuzzy?` (default `true` — case-insensitive substring; `false` = case-insensitive whole-run equality), `min_confidence?` (0–1; filters OCR-derived runs on mirror devices, no-op on tree backends whose text is exact), `timeout_ms?` (default 3000, max 120000), `interval_ms?` (default 500, min 10) | `asserted:"text"`, `found:true`, `text`, `matched` (the run that matched), `elapsed_ms`, `polls` |
| `assert_no_text` | same fields as `assert_text` | `asserted:"no_text"`, `absent:true`, `text`, `elapsed_ms`, `polls` |
| `assert_template` | `template_base64` (inline base64 PNG/JPEG template) or `name` (+ `bundle_id?`, default `_global`) — a stored template saved by `save_template`, looked up under the capture's device class; exactly one of the two — `threshold?` (NCC score in (0, 1], default 0.8), `timeout_ms?`/`interval_ms?` as above | `asserted:"template"`, `found:true`, `x`, `y` (best match top-left, capture pixels), `score`, `threshold`, `elapsed_ms`, `polls`, `name`/`bundle_id` (echoed for named lookups) |

Assertions read the backend's existing observe source — the a11y tree's
label/value/placeholder runs on simulators and WDA devices, the OCR text
boxes on mirror-backed devices (where `min_confidence` filters on real
recognition confidence) — and retry within a short bounded budget
(`timeout_ms`, default 3 s) so a screen still settling after an action
gets another look before the assertion fails. A postcondition that stays
false through the whole budget is `417 {"code":"assertion_failed"}` —
deliberately distinct from `timeout` (a wait that never matched): the
caller asserted a state and the daemon observed it to be false.
`assert_template` grayscales the current screen capture and the
template and matches by normalized cross-correlation
(`internal/imgutil.MatchNCC`); coordinates are native capture pixels.
Named lookups resolve against the template store (§5.1 `save_template`)
under the capture's device class (`"<backend>/<img_w>"`): a daemon
without `--templates-dir` is `503 unavailable`, and a name the store
lacks for that class is `404 not_found`.

`tap` and `swipe` additionally accept opt-in pixel verification via
payload `verify: true`: the backend captures a downscaled grayscale
frame through its existing screenshot path before the input and again
after a short settle (`verify_settle_ms?`, default 300, clamped to
10000), compares them
with a masked mean absolute pixel difference, and adds
`verified: true|false` plus the measured `pixel_delta` (0–1) and the
`pixel_threshold` applied (`verify_threshold?`, default 0.001) to the
result. Optional `verify_masks?` (array of `{x,y,w,h}` rects in native
capture coordinates) excludes volatile regions (clocks, spinners) from
the diff. Verification is a report, never a gate: a tap that
legitimately changes nothing returns `ok:true` with `verified:false`,
and a capture failure degrades to a `verify_error` field on a
successful action. The pixel evidence keys are distinct from the a11y
`hash`/`ax_before`/`ax_after` fields — they carry pixel deltas, never
tree hashes. On simulators a verified tap always runs through the cold
backend (the warm helper owns no screenshot path). On mirror-backed
devices the mirrord helper self-verifies every gesture, so it owns the
`verified` verdict and the daemon-level pixel wrapper is skipped (see
the iPhone Mirroring notes below, §5.2).

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
  `400 bad_request`. HID/observation kinds require an action backend for
  the device: WebDriverAgent by default (`--device-wda <udid>=<url>`),
  or the mirror backend (`--device-backend <udid>=mirror`, see below),
  otherwise `503 {"code":"unavailable"}`. Lifecycle kinds run through
  `devicectl` regardless of the action backend; a paired-but-unreachable
  device is `503 {"code":"unavailable"}` with a "device not connected"
  message.

| `kind` | payload | result |
|---|---|---|
| `install_app` | `path` (a `.app` on the daemon host) | `installed` |
| `launch_app` | `bundle_id`, `terminate_running?`, `args?` | `bundle_id` (no `pid`: devicectl does not report one) |
| `terminate_app` | `pid` (positive integer; devicectl terminates by process id, not bundle id) | `terminated_pid` |
| `tap` | `x`, `y` (>= 0), `duration_ms?` (number, 1–10000: a long-press via a W3C pointer chain, pointerDown → pause → pointerUp; out-of-range is `400 bad_request`) | `x`, `y`, `duration_ms?` (echoed when a long-press), `backend:"wda"` |
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
| `wait_for_stable` | same fields as the simulator kind | same result shape (frames via WDA screenshots) |
| `scroll_to_element` | same fields as the simulator kind | same result shape (swipes via WDA) |
| `screenshot` | same fields as the simulator kind | same result shape, `backend:"wda"` |
| `save_template` | same fields as the simulator kind | same result shape, `device_class` is `"wda/<img_w>"` |
| `tap_template` | same fields as the simulator kind | same result shape (capture and tap via WDA) |

> Device `observe` changed in this version: it previously returned the raw
> XML as `source`/`format`; it now returns the same compacted `tree` +
> `hash` shape as simulators (the raw XML is available via
> `include_raw`). This is the one non-additive device-result change in
> v0, made before any external consumer of the device surface existed so
> element predicates work identically across target kinds.

#### 5.2.1 Runtime device configuration (`/v0/devices`)

Device enumeration and WDA wiring are runtime-mutable (additive, v0):

- `GET /v0/devices` → `200` with the daemon's current `DevicesConfig`:
  `{"enabled": bool, "wda": {"<udid>": {"url"?, "launch"?, "forward"?}},
  "mirror"?: {"<udid>": {"socket"?, "allow_input"?}}}`
  (`url` the WDA endpoint, `launch` a `devicectl:<bundle-id>` or
  `xctestrun:<path>` auto-launch spec, `forward` a supervised
  `<local>:<remote>` usbmux port forward; `mirror` maps a device to the
  mirror action backend — `socket` is the mirrord helper's unix socket,
  default `~/.manzanasd/mirrord.sock`, and `allow_input` (bool, default
  false) opts the device into mutating mirror actions — the mirror is
  read-only without it, §5.3. At most one mirror-backed device
  per config, and a UDID cannot appear in both `wda` and `mirror`).
- `POST /v0/devices` with a `DevicesConfig` body **replaces** the whole
  runtime config: enumeration toggles, supervisors for removed/changed
  devices stop, added ones start — no daemon restart. Callers wanting
  merge semantics GET first. Returns `200` with the resulting config.
  Because the config's `launch`/`forward` specs make the daemon spawn
  host processes, this route requires the daemon to run with a shared
  bearer token (`--auth-token`) and returns `403 unauthorized` on a
  tokenless daemon; the devices config file + SIGHUP is the tokenless
  path. Unknown JSON fields are rejected (`400`) so a typoed key cannot
  silently detach everything.
  Validation failure is `400 {"code":"bad_request"}` and leaves the
  running config untouched. The change is runtime-only: the daemon's
  devices config file is not rewritten, and a later SIGHUP (which
  re-reads the file) supersedes it.
- Both routes return `501 {"code":"not_implemented"}` on daemons without
  runtime device management (e.g. `--mock`).
- With `"enabled": false` the config is remembered and echoed by GET,
  but no devices are enumerated, no WDA is wired, and no supervisor
  runs — equivalent to the legacy `--device-wda*` flags without
  `--devices`.

### 5.3 The mirror action backend

A device selected with `--device-backend <udid>=mirror` (or a `mirror`
entry in the devices config, §5.2.1) runs its
HID/observation kinds through macOS iPhone Mirroring (see
[docs/devices.md](../docs/devices.md)) instead of WDA. Lifecycle kinds
(`install_app`/`launch_app`/`terminate_app`) are unchanged. Differences
from the WDA table above (all additive within v0):

- Input is **fail-closed**: unless the device's mirror config sets
  `allow_input: true` (or the daemon runs with
  `--device-mirror-allow-input`), the mirror is read-only — the input
  kinds `tap` (including long-press), `swipe`, `scroll`, `type`,
  `button`, `tap_element`, `type_into_element`, `scroll_to_element`,
  and `tap_template`
  return `403 {"code":"forbidden"}` with a message naming the fix,
  while `observe`, `screenshot`, `wait_for_element`, and
  `wait_tree_stable` stay available. The gate tracks runtime config
  changes (`POST /v0/devices`, SIGHUP). `forbidden` is a well-known
  error code (additive, v0): the action kind exists and is configured,
  but policy disables it. The fail-closed default is a deliberate
  exception to the additive-within-v0 rule (a mirror drives a real,
  personal phone, so input must be an explicit opt-in): mirror configs
  written before `allow_input` existed lose input on upgrade until the
  operator opts in — see the migration note in
  [docs/devices.md](../docs/devices.md).
- Every result carries `backend:"mirror"` instead of `backend:"wda"`.
- `observe` and the composite element kinds (`tap_element`,
  `type_into_element`, `scroll_to_element`, `wait_for_element`,
  `wait_tree_stable`) additionally carry `fidelity:"ocr"` and declare
  reduced fidelity: the `tree` is a flat list of OCR-derived `Text`
  nodes (visible text + frame only — no roles, ids, values,
  placeholders, or hierarchy), `observe` includes an explanatory `note`,
  and `include_raw` returns the OCR boxes as `raw` with
  `raw_format:"vision-ocr-boxes"`. Element predicates can therefore only
  match on visible text. OCR nodes carry `interactable: true` as a
  uniform **compatibility constant** (kept for v0 wire-shape
  additivity), NOT a measurement — OCR cannot tell a button from a
  caption, so do not treat the flag as signal on mirror trees.
  `observe` with `interactive_only: true` therefore returns the full
  tree, appending an explanation to the `note` field. Coordinates
  (input and observed frames) are
  capture-image pixels, not device points. `observe` and `screenshot`
  results carry `img_w`/`img_h` — the pre-transform capture size, i.e.
  the tap coordinate space — so callers of size-capped screenshots can
  map back to tappable coordinates, plus `capture_scale` — the capture's
  pixels-per-window-point factor (the host display's backing factor;
  OCR quality degrades on 1x captures) — when the mirrord helper
  reports it (older helpers omit it).
- `wait_for_stable` polls mirrord window captures (no OCR pass per
  poll, so it is far cheaper than `wait_tree_stable` here); payload,
  result shape, and semantics match the simulator kind, with `mask`
  rectangles in capture-image pixels.
- `observe` accepts recognition options: `ocr_level` (`"fast"` |
  `"accurate"`; default the helper's, accurate) and `rect`
  (`{x,y,w,h}`, capture-image pixels — restricts recognition to that
  region; boxes stay in full-capture pixels). Malformed values are
  `400 bad_request`. The daemon also applies a resolution-aware
  auto-downscale default (long edge capped at ~1300px before
  recognition; the helper never downscales frames at or below 640px
  wide, so low-resolution 1x captures are never degraded). The result
  carries honesty metadata reporting what recognition actually ran:
  `served_scale` (resolution served to recognition relative to the
  capture, 1.0 = full), `ocr_level` (level applied), and `rect` (echoed
  only when one was requested). On a helper predating the options
  route, a default-only observe falls back to the legacy
  full-resolution pass (reported as `served_scale: 1`); explicitly
  requested `ocr_level`/`rect` are never silently dropped and fail
  `400 bad_request` instead. Caveat: the helper detects paused/connect
  interstitials by scanning the recognized text, so a `rect`-scoped
  observe can miss interstitial text outside the region and return a
  normal-looking (often empty) tree instead of `503 unavailable` —
  when a region-scoped observe comes back unexpectedly empty, re-observe
  without `rect` before concluding the screen is blank.
- `tap` accepts an optional `duration_ms` (number, 1–10000): a
  long-press held for that duration. Values outside the range are
  `400 bad_request` (the mirror helper is a single serial resource; an
  unbounded press would wedge every later action). The result echoes
  `duration_ms` — the mirror helper independently caps the duration it
  runs at 10 s and echoes the applied value in its ack, so the result
  reports what actually ran even for callers that bypass the daemon's
  validation. Simulators (touch-down → delay → touch-up) and WDA-backed
  devices (W3C pointer chain) implement the same field with the same
  bound — long-press works identically on every backend.
- `scroll` (mirror-only kind) posts wheel-scroll events at a point:
  payload `x`, `y` (>= 0), `dy` (non-zero; positive scrolls content
  down, like a trackpad two-finger-up); result echoes them plus
  `backend:"mirror"`. Units are capture-image pixels. Useful for fine
  list movement without the momentum a swipe gesture picks up. On
  simulators and WDA-backed devices `scroll` is
  `501 {"code":"not_implemented"}`.
  - `scroll` additionally accepts the optional `fallback` field, whose
    only value is `"swipe"` (anything else is `400 bad_request`). With
    the opt-in, a wheel scroll the helper refused *without sending
    anything* — `input-refused` (--background=on forbids a scroll that
    needs the focused path) or `focus-lost` (another app held the
    Mac's focus) — is delivered as the equivalent drag: a flick
    centered on (x, y) covering `dy` pixels (positive `dy` drags the
    finger up; a drag that would leave the capture is shifted back
    inside it, length preserved, since the helper bounds-checks both
    drag endpoints — one capture on this path learns the bounds). The
    result then carries
    `method: "swipe-fallback"`, the swipe geometry (`swipe_start_x`,
    `swipe_start_y`, `swipe_end_x`, `swipe_end_y`), a `note` naming the
    refusal, and the *swipe's* ack fields (`input_mode`, `verified`,
    `verify`, ...), so journals show what actually ran. Failures where
    events may have reached the phone (`input-not-delivered`,
    `scroll-not-delivered`, unverified acks) or where a swipe would fail
    identically (`blocked`, `no-window`, `not-running`) never trigger the
    fallback; without the opt-in every refusal surfaces unchanged. Additive: absent `fallback`
    is the pre-existing behavior.
- `tap`/`swipe`/`scroll` coordinates outside the current capture
  (`img_w`×`img_h`) are `400 bad_request`: an unbounded synthetic click
  would land on the host Mac's desktop, not the phone.
- `swipe` additionally rejects `duration_seconds` above 10 with
  `400 bad_request` (the mirror helper is a single serial resource).
- `type` and `type_into_element` reject the `paste` strategy and
  `require_focus` with `501 {"code":"not_implemented"}`; text is limited
  to characters expressible as US-layout HID keycodes (others are
  `400 bad_request`). Typing needs the mirroring window frontmost: iOS
  routes key events into text input only while iPhone Mirroring holds
  the Mac's focus (backgrounded keystrokes reach the process — the
  Cmd-shortcuts still work — but land in no text field), so on a
  background-capable host a `type` degrades per action to the focused
  path: the helper activates the window, types, then hands focus back
  to the app that previously held it (best-effort). The result carries
  `input_mode:"focused-degrade"` and an `input_note` explaining the
  focus grab and whether the previous app took focus back. Under
  `--background=on` (which forbids any activation) `type` is refused
  with `input-refused` and nothing is sent; if activation fails because
  another app cannot be displaced, the existing `focus-lost` refusal
  applies. The iOS on-screen keyboard never appears under mirroring
  (the mirror attaches a virtual hardware keyboard) — that is normal,
  not a focus failure.
- `button` supports only `home`; `lock`/`volume-up`/`volume-down` are
  `501 {"code":"not_implemented"}` (physical-phone functions the mirror
  cannot reach).
- `pasteboard_get`/`pasteboard_set` are `501 {"code":"not_implemented"}`
  (iPhone Mirroring exposes no pasteboard channel).
- Mirroring interstitials (not running, no window, "iPhone in Use"
  pause) surface as `503 {"code":"unavailable"}` with an actionable
  message naming the physical step required.
- The helper picks the mirroring window deterministically: the
  largest-area on-screen layer-0 window of the iPhone Mirroring
  process, cross-checked against the app's AX standard window when
  readable (an AX-confirmed candidate wins). The helper's `/v1/status`
  reports the chosen window's `window_id` (CGWindowID, additive) inside
  `window` for diagnosing wrong-window selection, and captures whose
  pixel size disagrees with the selected window's bounds fail loudly
  with `capture-failed` instead of silently desynchronizing capture
  from input.
- Input is delivered in one of these modes, and every input result says
  which in `input_mode`:
  - `background` (preferred): the helper posts events addressed to the
    iPhone Mirroring process without activating it, so driving the phone
    does not take focus away from whoever is using the host Mac.
  - `focused-degrade` (`type` only): a per-action degrade on an
    otherwise background-mode host — the window is activated for the
    duration of the typing (see the `type` bullet above) and focus is
    handed back best-effort, with the outcome named in `input_note`.
  - `focused` (fallback): the helper activates
    the mirroring window and posts at the HID tap, as before. In this
    mode input actions verify iPhone Mirroring is actually frontmost
    before posting events; if another app on the host Mac holds focus
    and the window cannot be brought frontmost (helper code
    `focus-lost`), the action fails with `503 {"code":"unavailable"}`
    and a message naming the app holding focus, and no input is sent.
    The daemon logs a loud warning when a device's input falls back to
    this mode. A per-action reason appears in the result's `input_note`.
  - `scroll` uses the background path only where the helper's
    scroll-record symbol resolves (`/v1/status` reports it as
    `background_scroll`, additive; false or absent means scroll is
    focused-only). Background scroll delivery is *best-effort* — the
    WindowServer intermittently accepts scroll records that move
    nothing on the phone — so the helper verifies each background
    scroll and re-delivers it on the focused path only after an extra
    late-render check proves no change (the result's `input_note` says so).
    Because the no-change verdict
    is fallible (the phone can render after the observation budget),
    the helper takes one more capture against the pre-gesture frame
    before re-delivering: a late change is returned as a verified
    background success (the note says the render was late) instead of
    replaying the burst and scrolling twice. Under `--background=on` the
    fallback is forbidden and the miss surfaces as helper code
    `scroll-not-delivered` (`503 {"code":"unavailable"}`), stating that
    the records were accepted but nothing observably changed. That is a
    posted-but-unproven failure, not a refusal: the records went out, so
    it never triggers the `fallback: "swipe"` substitution, and the
    scrolling composites count it as a non-move rather than flicking on
    top of it.
- Input actions are verified against the phone rather than assumed: the
  helper captures the window before and after the gesture and requires
  the touched region to change, re-observing with a longer settle on a
  miss (the gesture is posted exactly once, never replayed; the one
  exception is the background scroll fallback above, which is guarded
  by an extra late capture so a late render is never re-posted). Results
  carry `verified` (bool) and `verify` — `pixel-delta` (region/frame
  diff), `ocr-delta` (`type` only: newly recognized text matching what
  was typed appeared outside the status bar — typed glyphs move too few
  pixels for a reliable frame diff, but they change what OCR reads;
  unrelated text changes do not count and fall through to the pixel
  check), `off` (verification disabled at the helper), or `unavailable`
  (the check could not be completed) — plus `verify_delta`, the fraction of
  sampled pixels that changed, when a delta was measured. An action the
  helper posted but could not see land fails with
  `503 {"code":"unavailable"}`, either as the helper's typed
  `input-not-delivered` (all observations missed) or from the daemon
  refusing to report an unverified action as success. `verify:"unavailable"`
  fails too, but says something different: the events were posted and the
  check could not run, so delivery is *unknown* — re-observe before
  retrying a non-idempotent action rather than assuming nothing landed.
  Only `verify:"off"`
  (and helpers predating these fields) yield a success without evidence,
  and they say so. Distinct from both: `input-refused` means the helper
  sent *nothing* — `--background=on` forbade the only deliverable path
  (for example, a scroll with no background record under that flag) — a configuration
  limit, not a transient, so retrying without changing the helper's
  flags cannot succeed. The element composites (`tap_element`,
  `type_into_element`, `scroll_to_element`) enforce the same contract —
  an unverified gesture fails their whole action, except that the
  scrolling loops (`scroll_to_element`, `scroll_until`, `scroll_collect`)
  read a verified no-change *scroll* gesture as a confirmed end-of-list
  non-move rather than a failure (see §scroll_until above) — and their results
  carry the `input_mode`/`verified`/`verify` (and `verify_delta`/
  `input_note`, when present) fields of the composite's final gesture:
  the tap for `tap_element`, the keystrokes for `type_into_element`,
  the last swipe for `scroll_to_element` (a `scroll_to_element` that
  found its target already on screen posts no gesture and stamps no
  input fields).
- The helper's `/v1/status` additionally reports `background` (bool —
  focus-free input available), `input_mode`, `capture` (the active
  capture backend: `sck`, `cgwindowlist`, `screencapture`, or `none`),
  and `verify`. Status is cache-only: it never posts a probe event into
  the phone (it is polled by read-only listings, including for devices
  whose input gate is off), so before the first input or doctor run its
  `input_mode` may be `background?` — symbols present, delivery not yet
  demonstrated — with `background:false`. `GET /v1/status?light=1`
  answers the same shape (stamped `probe:"light"`) but skips the
  capture+OCR interstitial check — process/window presence only, so a
  blocked session reports `ready` — making it cheap enough for liveness
  polling on the helper's strictly serial worker. The target registry
  probes it routinely, paying for a full status about once a minute to
  keep blocked-interstitial detection (and while blocked, until the
  interstitial clears); helpers predating the light mode get the full
  status, with the light route re-tried about once a minute so an
  in-place helper upgrade is picked up without a daemon restart. This
  is why the `mirror-blocked` label (§2) can lag the interstitial by
  up to that interval. `GET /v1/doctor` returns the
  same fields plus `checks`, a list of `{name, ok, detail}` capability
  probes (real capture, real event record) rather than TCC database
  answers; the doctor's event-record probe is an explicit diagnostic
  and does post one harmless mouse-moved record.
- `assert_text`/`assert_no_text` read the OCR boxes, so `min_confidence`
  filters on real Vision recognition confidence, and results carry
  `backend:"mirror"` + `fidelity:"ocr"`. `assert_template` runs on the
  mirror window capture; coordinates and masks are capture-image pixels.
  For taps/swipes with `verify: true` the mirrord helper's own gesture
  verification owns the `verified` verdict (`verified`/`verify`/
  `verify_delta` from the input acknowledgement, and an unverified
  gesture already fails the action): the daemon-level pre/post pixel
  wrapper is skipped, so mirror results carry no
  `pixel_delta`/`pixel_threshold`, and the daemon-level tuning fields
  (`verify_threshold`/`verify_settle_ms`/`verify_masks`) have no effect.
  A helper whose own verification is off (or predates it) cannot answer
  `verify:true`; that degrades to a `verify_error` field on the
  successful action rather than a silent no-verdict result.
- Mirror-backed devices are enumerated in `/v0/targets` even on hosts
  devicectl knows nothing about, with mirror-derived liveness and the
  `mirror`/`mirror-blocked`/`no-tunnel`/`no-devicectl` labels (§2).
- `save_template` crops a fresh mirror window capture; its
  `device_class` is `"mirror/<img_w>"` and `rect` is in capture-image
  pixels (the same space as `tap`).

### 5.4 Template library (`/v0/templates`)

Templates saved by `save_template` live in a cross-run on-disk library
(default `~/.manzanasd/templates`, beside — never inside — the journal
directory, so journal GC cannot sweep it; `--templates-dir` moves it,
empty disables it). Two leaseless management endpoints (additive, v0):

- `GET /v0/templates` → `200 {"templates":[{"bundle_id","name",
  "device_class","window_px":[w,h],"crop_rect":{x,y,w,h},
  "source_run"?,"min_score_seen"?,"created_at","bytes","sha256"}]}` —
  every stored template, sorted by bundle, name, then class.
- `DELETE /v0/templates/{bundle_id}/{name}` →
  `200 {"bundle_id","name","removed":n}`. By default the name is removed
  across all device classes; `?device_class=<backend>/<img_w>` scopes
  the delete to one class. No match is `404 {"code":"not_found"}`.
- Both are `501 {"code":"not_implemented"}` on a daemon without a
  template library.

CLI: `manzanas templates ls` and
`manzanas templates rm BUNDLE_ID NAME [--class <backend>/<img_w>]`.

### 5.5 Element map (`/v0/elementmap`)

Elements resolved by successful `tap_element` / `type_into_element`
actions are learned into a cross-run on-disk **element map** (default
`~/.manzanasd/elementmaps`, beside — never inside — the journal
directory, so journal GC cannot sweep it; `--element-map-dir` moves it,
`--element-map=false` or an empty dir disables it; see
[docs/elementmap.md](../docs/elementmap.md)). Entries are keyed by
bundle id + device class (`<backend>/<img_w>`) + the element spec's
canonical matcher string, and carry the resolved frame, role, label,
and a DHash fingerprint of the screen at resolve time. The resolver
ladder's first rung (`map`) serves the cached frame directly — no
observe round-trip — when the current screen's DHash is within
threshold of the recorded fingerprint; results/journal entries carry
`resolver: "map"`. `refresh: true` bypasses the map rung (the flag asks
for cold, uncached observation). A tap or focus failure steered by the
map invalidates the entry. Two leaseless management endpoints (additive,
v0):

- `GET /v0/elementmap` → `200 {"elements":[{"bundle_id",
  "device_class","name","frame":{x,y,w,h},"role"?,"label"?,
  "screen_fingerprint","template_ref"?,"learned_at"}]}` — every
  learned element, sorted by bundle, name, then class.
- `DELETE /v0/elementmap/{bundle_id}?name=<matcher>` →
  `200 {"bundle_id","name","removed":n}`. Names are free-form matcher
  strings (`label="Sign In"`), so the name travels as a query
  parameter, not a path segment. By default the name is removed across
  all device classes; `&device_class=<backend>/<img_w>` scopes the
  delete to one class. No match is `404 {"code":"not_found"}`.
- Both are `501 {"code":"not_implemented"}` on a daemon without an
  element map.

CLI: `manzanas elementmap ls` and
`manzanas elementmap rm BUNDLE_ID NAME [--class <backend>/<img_w>]`.

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

`POST /v0/streams` (and its `OPTIONS` preflight) carries permissive CORS
headers (`Access-Control-Allow-Origin: *`): the broker's aggregated
dashboard is served from the broker's origin but negotiates streams
directly against each owning daemon. Only stream negotiation is
cross-origin-readable — the MJPEG media rides in an `<img>`, which needs
no CORS — and the v0 API carries no credentials to leak (the trust model
is the network boundary, unchanged).

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
and the post-lease auto-reset (`state.reset`). One-call runs (§9)
additionally journal their lifecycle as kind `run` (`runs.start` /
`runs.finish`).

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

## 9. Runs (owned by the runs slice)

The one-call run primitive: a single declarative request that executes the
whole canonical loop — acquire lease → boot → fixtures → install app →
launch → steps → artifact capture → release lease (applying its reset) —
so agents don't hand-sequence a dozen calls. Every stage reuses the
existing machinery: the lease manager (§3, including queueing and label
matching), target boot (§2), the state engine (§7) for fixtures/reset,
action dispatch (§5) for app lifecycle and steps, and the journal (§8)
for evidence. A run's journal is therefore indistinguishable from the
same loop done call-by-call; the journal run ID **is** the run's lease ID.

Wire types live in `proto/runs.go`; the YAML run-spec (same schema,
consumed by `manzanas run spec.yaml` and the MCP `run` tool) is documented
in [`docs/runs.md`](../docs/runs.md).

- `POST /v0/runs` with `{"spec": RunSpec, "agent_id": "...", "async": bool}`
  → sync (default): `200 Run` once the run finishes (bounded by the
  spec's `timeouts.run_seconds`, default 600 s, max 3480 s — the lease
  TTL cap minus the run's TTL margin); run-level
  failure is reported in `Run.state`/`Run.error`, not the HTTP status.
  With `"async": true` → `202 Run` (state `pending`) immediately; poll
  `GET /v0/runs/{id}`. Rejections before any lease is taken:
  `400 bad_request` (malformed spec, missing `agent_id` — `session_id`
  is accepted as an alias, like `leases.acquire`), `501 not_implemented`
  (reserved features: `target.image`, `maestro_flow` steps; or a spec
  needing a slice this build lacks), `503 overloaded` (too many runs in
  flight; carries `retry_after_seconds`).
- `GET /v0/runs` → `200 {"runs":[Run,...]}` — retained runs, newest
  first, without `export_md` (fetch a single run for the export).
- `GET /v0/runs/{id}` → `200 Run` | `404 not_found`. Run resources are
  in-memory (the durable record is the journal); finished runs are
  retained on a bounded LRU.

The broker serves the same three routes federated: `POST /v0/runs` places
the run on a candidate host (the same warm-first ranking as lease
scheduling) and proxies it there, `GET /v0/runs` is the union of all up
hosts' retained runs, and `GET /v0/runs/{id}` routes to the owning
daemon. Broker-returned runs additionally carry `host`/`host_addr`
(additive, like `Lease`) — see [`docs/broker.md`](../docs/broker.md).

`Run` reports `state` (`pending|running|passed|failed`), `stage` (which
stage is executing or failed: `acquire`, `boot`, `fixtures`, `install`,
`launch`, `steps`, `artifacts`, `release`), `lease_id` (= journal run ID),
`target_udid`, per-step results (`status` `ok|error|skipped`, wire `error`,
`duration_ms`, `journal_ref`), the first failure as `error`, and — once
finished, when `artifacts.export` is true (the default) and the journal is
enabled — `export_md`, the same markdown evidence summary as
`GET /v0/journal/{run}/export.md`.

The lease is always released (running its reset) when the run ends,
whatever failed; an abandoned queued acquire is released too. Steps run
with `stop-on-first-failure` semantics unless a step sets
`continue_on_error` (such steps still run after an earlier failure, and
their own failure doesn't halt later `continue_on_error` steps, but any
step failure makes the run `failed`).

### Run verdicts and replay drift (additive)

Once the steps stage ran, `Run` additionally carries `verdict` grading
how much of the run was actually *verified*, independent of
passed/failed:

- `trusted` — every executed mutating step's postcondition was verified
  (an opt-in `verify:true` report, a passing `expect` drift check, or a
  following passing `assert_*` step) and nothing failed verification.
- `degraded` — verification signals exist but coverage is incomplete or
  negative (unverified mutating steps, a `verified:false` report, a
  failed assert, or drift).
- `unverified` — the run carried no verification signals at all.

Mutating steps are everything that is not a pure read (the journal
exporter's read-only classification: `observe`, `screenshot`, `audit`,
`wait_*`, `assert_*`, ...); `batch` steps are conservatively mutating.
The verdict is mirrored additively into the journal: the `runs.finish`
entry's `verdict` field and the run meta's `verdict`.

Step results additively carry `verified` (mirroring the §5 action
result's opt-in verification report) and, when the step declared an
`expect` block, `drifted` plus `drift_detail`.

A step's optional `expect` block (`proto.RunStep.Expect`; YAML shape in
[`docs/runs.md`](../docs/runs.md)) declares its expected post-state:
`texts` (masked screen text set, compared by Jaccard overlap ≥
`jaccard_min`, default 0.7) and/or `fingerprint` (64-bit DHash hex of the
masked screen, compared by Hamming distance ≤ `max_hash_distance`,
default 10). After the step succeeds the engine re-observes the screen
through the normal §5 actions (`observe`/`screenshot`, journaled as
usual) and compares; a mismatch is report-only — `drifted:true` and a
verdict cap at `degraded` — unless the spec sets `strict_drift: true`,
which fails the run with `assertion_failed` at the first drifted step. A
capture failure skips the check (`drift_detail` explains) and never
fails the step.

Setting `record_expectations: true` on a spec journals a post-step
observation (kind `observation`, action `runs.step_expectation`, with
the masked `texts` set and `dhash`) after every successful mutating
step; `manzanas journal to-spec --with-expectations` turns those back
into `expect` blocks on the exported replay spec.
