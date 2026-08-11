# manzanas-broker: multi-Mac federation

`manzanas-broker` fronts N `manzanasd` daemons (one per Mac) behind a single
endpoint so agents can lease "any matching simulator in the fleet" without
knowing which Mac owns it. It is cross-platform — typically it runs on a
Linux box on the same tailnet as the Mac fleet.

## The broker is a scheduler, not a data-plane proxy

The broker only handles *placement*: target enumeration and lease
scheduling. Every lease it returns is annotated with `host` (the owning
host's name) and `host_addr` (the daemon's base URL); after acquiring a
lease through the broker, everything target-bound — boot/shutdown,
actions, streams, state, recording, the run journal — goes to `host_addr`
**directly**. The `manzanas` CLI and the MCP facade do this
**automatically**: their shared client caches `lease_id → host_addr` from
every lease it sees and routes lease-scoped calls to the owning daemon, so
pointing them at the broker address is all the configuration a fleet
needs (raw-HTTP clients must follow `host_addr` themselves). Media frames
and action traffic never flow through the broker, so it adds no latency
or bandwidth bottleneck and its failure never interrupts running work
(only new placements).

```
                        ┌ Mac host "emac" ──────────┐
   agents ──┐   lease   │ manzanasd :7433            │
            ▼  placement│                           │
   ┌ Linux box ───────┐ │                           │
   │ manzanas-broker   │─┤                           │
   │ :7440            │ │┌ Mac host "work" ─────────┤
   └──────────────────┘ ││ manzanasd :7433           │
            │           └┴───────────────────────────┘
            └── after grant: clients hit the owning
                daemon (host_addr) directly
```

## Running it

```sh
# flags: repeatable --host [name=]addr[,label,...]
manzanas-broker --addr :7440 \
  --host emac=http://100.64.0.1:7433,intel \
  --host work=http://100.64.0.2:7433,intel

# env (';'-separated specs)
MANZANAS_BROKER_HOSTS='emac=100.64.0.1:7433,intel;work=100.64.0.2:7433' manzanas-broker

# or a JSON config file
manzanas-broker --config fleet.json
```

`fleet.json`:

```json
{"hosts": [
  {"name": "emac", "addr": "http://100.64.0.1:7433", "labels": ["intel"]},
  {"name": "work", "addr": "http://100.64.0.2:7433", "labels": ["intel"]}
]}
```

Host sources are merged (config file, then env — env is ignored when
`--host` flags are given — then flags); names and addresses must be unique.

## Firewalled hosts: tunneled addresses

A daemon behind a firewall that blocks inbound `:7433` (e.g. the macOS
application firewall before its allow rule is in place) can be fronted
through an SSH tunnel: forward a local port on the broker host to the
daemon's loopback, then configure the host with a `localhost` address —
tunneled hosts are ordinary hosts, so direct and tunneled entries mix
freely:

```sh
ssh -N -L 127.0.0.1:7435:127.0.0.1:7433 user@firewalled-box &

manzanas-broker --addr :7440 \
  --host emac=http://100.64.0.1:7433,intel \
  --host trashcan=http://localhost:7435,intel   # tunneled
```

Everything works unchanged — health probes, target refresh, and lease
proxying all go through the tunnel. The one caveat: `host_addr` on granted
leases is the configured address, so clients resolve `localhost:<port>`
relative to *themselves*. Tunneled hosts therefore work out of the box when
clients run on the broker host; clients elsewhere need the same tunnel (or
direct reachability) locally.

Prefer fixing the firewall (direct is the primary path); for tunnel
supervision — auto-reconnect, idempotent startup, reboot persistence —
wrap the ssh command in a LaunchAgent, and allow the daemon binary with
`/usr/libexec/ApplicationFirewall/socketfilterfw`.

## Health checking

Every host is probed on an interval (`--probe-interval`, default 10s;
per-probe timeout `--probe-timeout`, default 3s): `GET /v0/healthz`, a
target-list refresh, plus a `GET /v0/status` fetch of the daemon's own
load/occupancy snapshot (capacity, running/parked counts, lease counts,
load/disk gates). Daemons that don't serve `/v0/status` (older builds)
stay fully supported — they simply report no stats and keep the
pre-status scheduling behavior. If healthz succeeds but the
target-list refresh fails, the host stays up on its cached target list
and the failure is surfaced in `last_error`. A host that fails its probe
is marked **down**: its targets vanish from the federated list and it receives no new leases until
a probe succeeds again. Existing leases on a down host are untouched — the
daemon owns them, and clients holding `host_addr` may keep working if the
outage is only broker→daemon.

`GET /v0/fleet/hosts` reports the fleet:

```json
{"hosts": [
  {"name": "emac", "addr": "http://100.64.0.1:7433", "labels": ["intel"],
   "up": true, "last_probe": "2026-08-01T04:00:00Z", "targets": 3, "active_leases": 1,
   "stats": {"capacity": {"max_booted_running": 2, "max_parked": 6, "max_concurrent_boots": 1},
             "running": 1, "parked": 2, "boots_in_flight": 0,
             "leases_active": 1, "leases_queued": 0,
             "load_avg1": 1.4, "cpus": 8, "free_disk_bytes": 91234567890,
             "gates": {"load_ok": true, "disk_ok": true}},
   "stats_at": "2026-08-01T04:00:00Z"},
  {"name": "work", "addr": "http://100.64.0.2:7433",
   "up": false, "last_error": "dial tcp ...: connect: connection refused",
   "last_probe": "2026-08-01T04:00:00Z", "targets": 0, "active_leases": 0}
]}
```

## Wire surface

Where the broker's surface overlaps the daemon's, it speaks the same v0
protocol — a thin client pointed at the broker for targets/leases just
works:

| Route | Behavior |
|---|---|
| `GET /v0/healthz` | `{"ok":true,"version":"v0","role":"broker","hosts":N}` |
| `GET /v0/targets` | union of all healthy hosts' targets, each annotated with `host` and the host's extra labels, in stable host order |
| `POST /v0/leases` | federated acquire (below) |
| `GET /v0/leases` | union of all healthy hosts' lease listings, each annotated with `host`/`host_addr` |
| `GET/POST/DELETE /v0/leases/{id}[/renew]` | proxied to the owning daemon by lease ID |
| `POST /v0/runs` | federated one-call run: placed on a candidate host (same warm-first ranking as leases) and proxied there; the returned run carries `host`/`host_addr` (below) |
| `GET /v0/runs` | union of all healthy hosts' retained runs, newest first, annotated with `host`/`host_addr` |
| `GET /v0/runs/{id}` | proxied to the daemon the run was placed on (falling back to asking each up host after a broker restart) |
| `GET /dash/` | broker-specific: embedded aggregated fleet dashboard (below) |
| `GET /v0/fleet/hosts` | broker-specific: host list + health |
| `GET /v0/fleet/placements` | broker-specific: recent placement decisions with ranked candidates (below) |
| `GET /v0/fleet/hints` | broker-specific: current warm-pool rebalancing hints per host (below) |

Unknown routes and wrong methods answer with the protocol's JSON error
envelope (`404 not_found` / `405 bad_request`), matching every other
error shape.

Anything else (boot, actions, streams, state, recording, journal) is
*not* served by the broker — it answers `404 not_found` with message
`unknown route`. The bundled clients recognize that answer, resolve the
lease's `host_addr` (via `GET /v0/leases/{id}` on the broker), and retry
against the owning daemon transparently; a fresh process (each CLI
invocation) recovers the route on its first lease-scoped call. Reading
the journal of a *finished* run whose lease the broker has already
forgotten falls back to trying each up host from `/v0/fleet/hosts`.
Raw-HTTP clients should use the lease's `host_addr` directly.

## Fleet dashboard

The broker serves a built-in aggregated dashboard at
`http://<broker-addr>/dash/` — one page showing every up simulator across
the whole fleet. Like the daemon's dashboard it ships embedded in the
`manzanas-broker` binary (no extra install, no new flags):

```sh
manzanas-broker --addr :7440 \
  --host emac=http://100.64.0.1:7433,intel \
  --host work=http://100.64.0.2:7433,intel
# open http://<broker>:7440/dash/
```

- **Fleet tab** — per-host health (up/down with the probe's `last_error`,
  labels, target and lease counts, last probe time) plus the federated
  targets and leases tables, each row carrying its owning host. A "live
  view" button per booted target opens the owning daemon's `/view/{udid}`
  page. A down host is shown as down while every other host keeps working.
- **Multiview tab** — one tile per booted, un-parked simulator target
  across ALL hosts. Same resource model as the daemon dash: streams are
  strictly opt-in per tile (click to start/stop, start all / stop all),
  never always-on.

The broker dash is **read-only** (no boot/shutdown/release controls) and
polls the broker's own endpoints (`/v0/fleet/hosts`, `/v0/targets`,
`/v0/leases`) every 5 seconds — the broker has no WS surface, and those
listings are backed by cheap JSON GETs against each daemon.

The scheduler-not-data-plane rule holds: clicking a tile negotiates the
stream by POSTing `{host_addr}/v0/streams` **directly against the owning
daemon** and attaches the offer's MJPEG URL on that daemon — media never
flows through the broker. (That cross-origin negotiation is why the
daemon's `POST /v0/streams` carries permissive CORS headers; see
[PROTOCOL.md §6](../proto/PROTOCOL.md).) Trust model is unchanged: no
auth, tailnet-only — see [dashboard.md](dashboard.md).

## Federated runs

`POST /v0/runs` ([docs/runs.md](runs.md)) works against the broker like
against a daemon: the broker derives placement labels from the spec's
`target` (labels plus the slugs from `runtime`/`device_type`; host-level
labels pin the run to one Mac — alongside at least one target selector,
since the daemon still needs one to pick a target on that Mac — and a
pinned `udid` restricts candidates
to hosts owning it), ranks candidate hosts exactly like a lease acquire
(warm-first — a run *is* a lease acquire plus choreography), and proxies
the run to the winner. The run executes entirely on the owning daemon —
the broker only routes. Sync requests are served by submitting the run
async to the daemon and polling it until the run finishes, so a
multi-minute run never holds a long broker→daemon HTTP call; `async`
requests answer `202` immediately and `GET /v0/runs/{id}` polls through
the broker. Spec problems (`400`/`501`) are forwarded from the daemon
verbatim; capacity refusals (`503 overloaded`) try the next candidate.
The returned run carries `host` and `host_addr` so raw clients can reach
the run's journal on the owning daemon directly.

## Federated leasing

`POST /v0/leases` takes the same `AcquireLeaseRequest` as a daemon. Labels
may include **host-level labels**: each host's name (`emac`) and its
configured extra labels (`intel`) are matchable alongside target labels
(`ios26`), so `["emac","ios26"]` pins the lease to one Mac. Host-level
labels are stripped before the acquire is proxied (daemons only know
target-derived labels).

Scheduling:

1. Candidate hosts = healthy hosts whose (cached) target list has at least
   one target carrying all requested labels. None → `409 no_match` — unless
   no host is up at all, or the labels select a configured host that is
   currently down; both are `503 unavailable` (a retryable outage, not a
   label mismatch).
2. Candidates are ranked **warm-first**, in three tiers:
   - **Tier 1 — warm/booted match:** a target carrying all requested
     labels is parked-warm in the daemon's pool or already Booted —
     granting there needs a ~26ms thaw, not a multi-minute cold boot.
   - **Tier 2 — cold-boot headroom:** the daemon reports positive
     running headroom (`max_booted_running − running − boots_in_flight`)
     and passing load/disk gates. Hosts that report no stats (older
     daemons) also sit here.
   - **Tier 3 — saturated:** no headroom and no warm match (a new
     acquire is guaranteed to queue), or failing gates (a cold boot
     would be refused). Ranked last but never skipped — the daemon
     stays the final authority.

   Within a tier, hosts are ordered by ascending **effective load**: the
   daemon's own `leases_active + leases_queued` from its last status
   probe (which sees direct-to-daemon clients too), bumped by any leases
   the broker granted since that probe; hosts without stats fall back to
   the broker-local counter. Equal-load hosts are then ordered by
   descending **idle warm depth** (matching parked-warm targets), so
   demand is steered toward the host with the most idle warm capacity
   for the class and thin warm pools stay free for their own classes. A
   rotating tiebreak spreads hosts that are still equal after all keys.
3. The first host that grants an **active** lease wins → `201` with
   `host`/`host_addr` set.
4. If every candidate queues the request, the broker keeps the queued
   lease on the candidate with the **shallowest daemon-reported queue**
   (`leases_queued`; hosts without stats rank last) — releasing the
   other speculative queued leases — and returns `202` — queueing itself
   is owned by that daemon's FIFO queue. Poll `GET /v0/leases/{id}` **through the broker**
   until active. When the acquire carries a reset spec (`erase`/
   `snapshot:<name>`), a speculative queued lease's state is re-checked
   before it is released; one the daemon promoted in the meantime is
   adopted as the winning grant instead (releasing it would run the reset
   and erase a simulator nobody used).

The broker remembers lease→host, so renew/release/get by lease ID route to
the right daemon; terminal leases (released/expired) stop counting as load
but keep a routing tombstone for the daemon's terminal-retention window,
so terminal reads and repeated (idempotent) releases behave the same
through the broker as against the daemon. Each probe round also reconciles the table against the
daemons, forgetting leases that ended behind the broker's back (TTL
expiry, direct release against `host_addr`, daemon GC) so load counters
and `/v0/fleet/hosts` stay accurate. Broker restarts lose that table — daemons still expire the
leases by TTL, and clients that saved `host_addr` can renew/release
against the daemon directly — the bundled CLI/MCP clients do exactly
that automatically: lease control ops (get/renew/release) go through the
broker while it answers, and fall back to the cached `host_addr` when the
broker is unreachable or has forgotten the lease.

Broker-local load counters still exist as the fallback for daemons
without `/v0/status`, and as the intra-probe-interval bump for fresh
grants; daemons that do report status make direct-to-daemon clients
visible to placement at the next probe. The daemon's own queue always
serializes correctly either way. Run one broker per fleet.

## Placement observability

Every federated acquire records a **placement decision**: the requested
labels, the outcome (`active`, `queued`, or an error code), the winning
host and its tier, a one-way lease-ID digest (the ID's holder can compute
the same digest to correlate; nothing of the token leaks), and the full
ranked candidate list the decision walked — per
host: tier, effective load, warm match/idle-warm depth, and (when the
daemon reports status) parked count and running headroom. The broker
retains the most recent 256 decisions in memory.

`GET /v0/fleet/placements[?n=N]` returns them newest-first:

```json
{"placements": [
  {"at": "2026-08-03T19:00:00Z", "labels": ["ios26"], "class": "ios26",
   "outcome": "active", "host": "emac", "tier": "warm", "lease_id": "sha:1a2b3c4d",
   "candidates": [
     {"host": "emac", "tier": "warm", "effective_load": 0, "warm_match": true,
      "warm_idle": 2, "has_stats": true, "parked": 2, "headroom": 1},
     {"host": "work", "tier": "headroom", "effective_load": 1, "warm_match": false,
      "warm_idle": 0, "has_stats": true, "headroom": 2}]}
]}
```

The same view is available from the CLI, pointed at the broker:

```sh
manzanas --daemon broker-box:7440 fleet hosts        # host health at a glance
manzanas --daemon broker-box:7440 fleet placements   # why each lease went where
manzanas --daemon broker-box:7440 fleet hints        # current rebalancing advice
```

## Warm-pool rebalancing (advisory)

The broker watches its placement decisions over a sliding demand window
(default 10 minutes) and derives per-host **hints**:

- **grow** — a label class fell to cold tiers (headroom/saturated) at
  least 3 times in the window: every up host whose targets match the
  class is advised to keep it warm, with the windowed cold-placement and
  warm-hit counts attached.
- **shrink** — a host reports parked warm capacity that served zero warm
  placements across the whole window while the fleet saw real demand
  (≥3 placements): the pool may be warming the wrong things. A quiet
  fleet never draws shrink advice, nor does a host that hasn't been up
  for a full window (it never had a shot at the demand) or one that is
  simultaneously being advised to grow.

Hints are exposed two ways:

- **Pull**: `GET /v0/fleet/hints` on the broker (daemons, dashboards, or
  operators can poll it).
- **Push**: after each probe round the broker POSTs changed hints to each
  daemon's `POST /v0/pool/advise` (once per change, not per round).

`POST /v0/pool/advise` on the daemon takes
`{"source":"broker","window_seconds":600,"classes":[{"labels":["ios26"],"action":"grow","cold_placements":5}]}`
and answers `{"accepted":true,"acted":false}`.

**Advice is never binding.** The daemon only *records* it — surfaced on
`GET /v0/status` as `pool_advice` for operators — and always retains
final say over its warm pool through its own capacity class and
load/disk gates; nothing is booted, parked, or evicted on a scheduler's
word. Old daemons without the endpoint answer 404, which the broker
remembers (per host, until the host bounces) and skips — fully backward
compatible. Placement-time steering (warm-first tiers + idle-warm-depth
ordering above) works regardless, so rebalancing degrades gracefully to
demand steering on mixed-version fleets.

## Limits (v0.1)

- No auth (same as the daemon) — tailnet-only, never expose publicly.
- No WS surface on the broker; use REST for placement, then the owning
  daemon's WS.
- Static host list; add/remove hosts by restarting the broker.
