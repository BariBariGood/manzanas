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
negative disables). `503 overloaded` is always retryable — the response
carries a `Retry-After` header and `retry_after_seconds` in the body;
wait at least that long instead of busy-polling. Or boot with
`?wait=true` and let the daemon retry the boot server-side (up to 10
minutes) and answer once it is accepted.

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
  resolvable for 10 minutes, then become `404 not_found`. A renew that
  arrives up to the renewal grace window after nominal expiry (default
  2 minutes; `--lease-renew-grace`) still succeeds — watch for the
  `lease.expiring` WS event or a negative `expires_in_seconds`.
- **`409 target_not_booted` mid-lease** — the leased sim is shut down.
  The error's `detail` says who the daemon thinks shut it down and why
  (janitor, watchdog, dashboard, another agent via simctl); re-boot the
  target with your lease and continue.
- **`408 timeout` on `wait_*` / `tap_element`** — the predicate never
  matched within `timeout_ms`. Grab an `observe` (or `screenshot`) and
  check what the tree actually contains.
- **`observe` returns `tree: []` with `detail: "empty_tree"`** — the
  a11y snapshot compacted to nothing even after the daemon's bounded
  in-process retries. On React Native screens this is usually the a11y
  bridge re-attaching, not a truly blank screen: re-observe (or use
  `wait_for_element`, which treats empty snapshots as "not yet" — they
  never satisfy `absent:true` either).
- **`412 focus_required` on `type` / `type_into_element`** — the action
  ran with `require_focus:true` and no on-screen keyboard appeared, so
  the daemon refused to send keystrokes. Tap a text field first (or fix
  the predicate); on sims with *Connect Hardware Keyboard* active the
  on-screen keyboard never shows — drop `require_focus` there.

## Typing wedges the app in AFDictationConnection (slimmed sims, iOS 26.5)

Symptom: after a `type` action on a slimmed sim the app freezes; a spindump
shows the **main thread** blocked in
`AFDictationConnection`/`UIDictationController`.

Cause: iOS 26.5's hardware-keyboard path synchronously consults the speech
daemons on the first keystroke; a slim profile that disables
`com.apple.assistantd`/`com.apple.corespeechd` (simslim's `siri` category)
leaves that XPC call hanging.

Fixes (either works):

- Slim QA pool sims with a profile that keeps the speech daemons — add
  `"except": ["siri"]` or
  `"keep": ["com.apple.assistantd", "com.apple.corespeechd"]` (the
  reference `agent-qa` profile keeps them). `manzanasd` logs a startup warning when
  `--pool-slim-profile` names a profile without them.
- Type with `strategy: "paste"` (PROTOCOL.md §5.1): pasteboard + one
  Cmd-V chord, no per-keystroke hardware-keyboard events at all.

## Stray keystrokes reload the React Native app (Metro `r`)

Symptom: mid-QA the RN app suddenly reloads and the flow starts over.

Cause: keystrokes sent while **no text field is focused** don't vanish —
the sim's hardware-keyboard events land wherever key commands are
listening, and RN dev builds treat `r` as "reload" (Metro's dev menu key
bindings). A mistargeted `type`/`key` — e.g. after a failed focusing tap —
can silently reload the app.

Fixes:

- Pass `require_focus: true` on `type` / `type_into_element` for QA
  driving: the daemon verifies the on-screen keyboard is up before any
  keystroke and fails with `412 focus_required` instead of spraying keys.
- Prefer `type_into_element` (tap-then-type in one action) over blind
  `type`, and `strategy: "paste"` for anything long.
- Not applicable on sims with *Connect Hardware Keyboard* — the on-screen
  keyboard never shows there, so `require_focus` would always fail.

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

## Debug RN app loads another project's JS (wrong Metro on :8081)

Symptom: a debug React Native / Expo app installed and launched through the
daemon shows a different app's UI, or red-screens about unknown modules.

Cause: a debug RN app attaches to whatever Metro answers on
`localhost:8081` on the Mac — on shared fleet Macs that is usually another
repo's long-lived packager. The
daemon launches the app as-is; it does not (and cannot) know which Metro
is yours.

Fix: pin the app to your Metro's port via the `RCT_jsLocation`
NSUserDefaults key (read by `RCTBundleURLProvider` at app launch), then
relaunch the app:

```sh
xcrun simctl spawn <udid> defaults write <bundle-id> RCT_jsLocation "localhost:<port>"
```

Delete the key (`defaults delete <bundle-id> RCT_jsLocation`) or erase
the sim when you finish so the next agent isn't pinned to a dead port.

## Daemon / install

- `curl -s localhost:7433/v0/healthz` — liveness; version via
  `~/bin/manzanasd --version`.
- Logs: `~/.manzanasd/logs/manzanasd.{out,err}.log` (LaunchAgent install,
  [install.md](install.md)); rotation is automatic.
- Two daemons against one CoreSimulator set corrupt each other's pool
  bookkeeping — one daemon per Mac ([fleet.md](fleet.md)).
