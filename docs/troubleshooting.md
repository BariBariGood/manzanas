# Troubleshooting

Symptoms → causes → fixes, for operators and agents. Error codes are the
protocol's JSON envelope (`{"code","message"}`, [PROTOCOL.md](../proto/PROTOCOL.md) §1).

## `503 overloaded` on boot

A host safety gate refused the boot ([warm-pool.md](warm-pool.md)):

- **running-sim cap** — too many Booted un-parked sims (2 Intel / 4 Apple
  Silicon by default). Release a lease / shut a sim down, or wait.
- **load gate** — 1-min load > 2× cores. Something heavy (a build) is
  running; wait for it.
- **free-disk gate** — under 5 GiB free on the sim volume. Delete old
  sims/DerivedData or grow the disk.

The daemon log names the gate. All three are tunable
(`--pool-max-running`, `--pool-load-factor`, `--pool-min-free-disk-gb`;
negative disables). `503 overloaded` is always retryable — back off and
retry rather than escalating.

`overloaded` also appears on the WS surface when a connection pipelines
too many actions, or has 4+ `images.*` operations in flight — same
answer: back off, retry.

## Boot failures

- **Boot accepted (`202`) but the target never reaches `Booted`** — the
  boot died in the background. The *next* boot request for that target
  surfaces the stored failure as a `500`; retrying after that proceeds
  normally. Check `xcrun simctl list devices` and the daemon log.
- **`409 target_not_booted` on an action** — the leased sim is shut down;
  `POST /v0/targets/{udid}/boot` first (or use the eval harness's `boot`
  step / `manzanas boot`).
- **First boot after stamping is slow** — expected once per sim (data
  migration); golden-image stamping already skips the ~25 s first-boot
  migration for the archived state.
- **Boots time out on Intel under parallel load** — boots are serialized
  by the capacity class on purpose; don't fight it, queue behind a lease.

## Parked pool sims (what "parked" means)

Pool members are kept SIGSTOPped between leases ([warm-pool.md](warm-pool.md)):

- They report `Booted` in `/v0/targets` and `simctl list` — the tree is
  alive but frozen at ~0 CPU. This is normal, not a hang.
- **Do not drive a parked sim out-of-band** (raw `simctl`/AXe over SSH):
  it won't respond until thawed. Acquire a lease — the grant thaws it in
  ~26 ms (AS) / ~225 ms (Intel).
- Streams refuse parked sims (`409 target_busy`) — capture against a
  frozen tree would hang. Lease the sim to thaw it, then open the stream.
- If a daemon died uncleanly, restart it: startup recovers frozen trees
  it owned. Never `kill -CONT` another daemon's trees by hand unless the
  daemon is truly gone.

## Quarantined targets

A failed post-lease reset (or failed pool rebuild) quarantines the target
— it is never handed out dirty and queued leases stay queued. Fix the
underlying issue (daemon log has the simctl error), then
`POST /v0/targets/{udid}/clear-quarantine`. `409 target_busy` from
clear-quarantine means the reset is still running (resets are bounded by
a 10-minute timeout; slim re-apply can push slightly past it).

## Actions

- **`503 unavailable`** — AXe is not installed (install
  [AXe](https://github.com/cameroncooke/AXe) at `~/bin/axe`, or point
  `--axe` at it), or the a11y bridge stayed unready through the retry
  budget (common for a moment right after an app launch — retry).
- **Actions are slow (~1–3 s each)** — you're on the cold path (one AXe
  spawn per action). Build/install `simbridge` for the warm path
  (~130 ms end-to-end taps vs ~1050 ms cold; see
  [actions-warm.md](actions-warm.md)). Check the daemon startup log:
  `warm actions backend ready` vs `simbridge not found`.
- **`410 lease_expired`** — renew at ~half TTL; terminal leases stay
  resolvable for 10 minutes, then become `404 not_found`.
- **`408 timeout` on `wait_*` / `tap_element`** — the predicate never
  matched within `timeout_ms`. Grab an `observe` (or `screenshot`) and
  check what the tree actually contains.

## Leases

- **`409 no_match`** — no target carries all requested labels (typo, or
  the pinned `udid` doesn't match). `GET /v0/targets` shows each target's
  labels.
- **Stuck in `queued`** — you must poll `GET /v0/leases/{id}` at least
  every 30 minutes or the queued lease is dropped; events alone don't
  keep it alive.
- **Broker `503 unavailable` on acquire** — every candidate host is down
  (retryable outage), vs `409 no_match` (no host has such a target). See
  [broker.md](broker.md).

## Streams

- **`409 target_busy` on open** — target not Booted, or parked (above).
- **`429 stream_limit` / `viewer_limit`** — at `--stream-max-streams` /
  `--stream-max-viewers`; close viewers or raise the caps.
- **~1 fps on Intel** — expected: capture is paced `simctl screenshot`
  (~0.8 s/frame on 2017 Intel). The configured fps is an upper bound
  ([streaming.md](streaming.md)).

## Journal

- **`501 not_implemented` on `/v0/journal`** — the daemon runs with the
  journal disabled (`--journal-dir ""`), or the run's `meta.json` declares
  a format this daemon doesn't know.
- **`409 lease_expired` on artifact upload** — the run's lease ended; the
  journal is immutable after that.

## Daemon / install

- `curl -s localhost:7433/v0/healthz` — liveness; version via
  `~/bin/manzanasd --version`.
- Logs: `~/.manzanasd/logs/manzanasd.{out,err}.log` (LaunchAgent install,
  [install.md](install.md)); rotation is automatic.
- Two daemons against one CoreSimulator set corrupt each other's pool
  bookkeeping — one daemon per Mac ([fleet.md](fleet.md)).
