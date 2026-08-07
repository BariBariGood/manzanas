# Agent QA via manzanasd: a worked end-to-end session

How an agent runs a real app QA pass against a live daemon, start to
finish, using only manzanasd — no raw `ssh` + `simctl`/AXe. Every step
below was executed against a real fleet Mac; timings are from that run.

Conventions used here:

```sh
export MANZANASD_ADDR=<mac-tailnet-ip>:7433   # or 127.0.0.1:<port> over an SSH tunnel
export MANZANAS_LEASE=<lease-id>              # set after acquire; all actions read it
```

If the Mac's application firewall resets raw tailnet connections to the
daemon (symptom: `curl` gets `Connection reset by peer` while the same
request works on the Mac itself), tunnel instead:
`ssh -f -N -L 17433:localhost:7433 <mac>` and use `127.0.0.1:17433`.

## 1. Lease a simulator

```sh
manzanas targets                        # list UDIDs, runtimes, labels, states
manzanas lease acquire --labels iphone-17 --agent my-agent --ttl 3600 \
    --udid <UDID>       # optional: pin a specific target
    --reset erase       # optional: auto-erase when the lease ends
```

`--agent` sets the wire field `agent_id` — the caller identity on the
lease (the raw API also accepts `session_id` as an alias for it).

- Without `--udid` the daemon picks **any** matching target — including
  sims other (non-manzanasd) agents created. On shared Macs check
  `/tmp/manzanas_locks/` first and pin your own sim with `--udid`.
- `--reset erase` is the "release with reset" pattern: on release or
  expiry the daemon erases (and re-parks, for pool sims) the target
  before handing it to the next holder. Nothing to remember at teardown.
- A lease can come back `queued` even when the pinned sim looks free —
  e.g. while the warm pool is still parking it after daemon startup
  (initial park ≈ 2 min with a slim profile). Poll
  `manzanas lease wait <id>` (or `GET /v0/leases/{id}`) until `active`.
- Renew long sessions: `manzanas lease renew <id> --ttl 3600`. An expired
  lease invalidates every in-flight action with `410 lease_expired`.

## 2. Boot (thaw) and install the app

```sh
manzanas boot <UDID> --lease $MANZANAS_LEASE   # async: returns current state
manzanas targets            # poll until Booted (pool thaw: ~0s; cold boot: 30-90s)
manzanas app install /path/on/the/mac/MyApp.app
manzanas app launch com.example.myapp
```

- Boot is a **REST route, not an action kind**: over raw HTTP it is
  `POST /v0/targets/{udid}/boot` with `{"lease_id":"..."}` (the
  `manzanas boot` CLI wraps exactly that). Sending `{"kind":"boot"}` to
  `POST /v0/actions/{udid}` is rejected — `/v0/actions` only carries the
  interaction kinds in PROTOCOL.md §5 (tap, type, observe, …).
- `boot` is asynchronous — poll `manzanas targets` (or `--json` and check
  `state`) until `Booted`. Pool sims thaw in ~0–3 s; cold boots on Intel
  can take minutes and are subject to the daemon's load/disk/cap gates
  (refusals come back as `429 capacity`).
- The `.app` path is a path **on the daemon's Mac**, not on your client.
  For Expo/RN debug builds, Metro must be reachable from the sim
  (typically `localhost:8081` on the same Mac) or the app boots to the
  "No script URL provided" screen.
- **Don't trust the default `:8081` attach on shared Macs** — the app
  binds to whichever Metro owns the port, which may be another repo's.
  Pin it first: `xcrun simctl spawn <udid> defaults write <bundle-id>
  RCT_jsLocation "localhost:<port>"`, then relaunch (see
  [troubleshooting.md](troubleshooting.md), "wrong Metro on :8081").

## 3. Drive the UI: observe, composite actions, batches

One-shot actions (all honor `$MANZANAS_LEASE`):

```sh
manzanas observe                          # compacted a11y tree + stable hash
manzanas tap 200 400
manzanas type 'hello'
manzanas swipe 200 600 200 200 --duration-ms 300
manzanas button home
manzanas screenshot -o step.jpg --format jpeg --max-dim 800
```

Composite actions (HTTP/WS payloads; no per-step observe round trip):

- `tap_element` / `type_into_element`: find by predicate
  (`label`/`role`/`value`/`id`, `exact`, `in_frame`), tap centre, type.
- `wait_for_element`: poll until a predicate matches (or `absent`).
- `wait_tree_stable`: wait for the a11y tree hash to stop changing —
  the right "screen settled" barrier after taps/launches.

Multi-step flows go in **one batch** per screen transition
(`POST /v0/actions:batch`, WS `actions.batch`):

```sh
curl -s -X POST "http://$MANZANASD_ADDR/v0/actions:batch" -d '{
  "lease_id": "'$MANZANAS_LEASE'",
  "stop_on_error": true,
  "actions": [
    {"kind":"tap_element",      "payload":{"label":"Continue"}},
    {"kind":"wait_tree_stable", "payload":{"timeout_ms":8000}},
    {"kind":"screenshot",       "payload":{"format":"jpeg","max_dim":800}}
  ]
}'
```

Gotchas that cost real time:

- Batch entries take `payload`, **not** `params` (the PROTOCOL.md tables
  name the fields, the JSON key is `payload`). An unknown top-level key
  such as `params` is rejected with `400 bad_request` naming the field —
  see "Batch entries are strict about their keys" below. Still prefer
  `stop_on_error: true` and check per-entry `ok`.
- Inline screenshots come back base64 **inside the action result
  envelope** — `result.jpeg_base64` / `result.png_base64` (key follows
  `format`), not top-level. `--format jpeg --max-dim 800` turns a
  ~270 KB PNG into ~30-60 KB — use it for every evidence shot.
- First action after boot pays the a11y bridge warm-up (~3-5 s cold,
  ~0.4 s once the warm helper is resident). `observe` retries the
  "No translation object returned" window internally.

## 4. Stream (watch the run live)

```sh
manzanas stream url --lease $MANZANAS_LEASE     # browser view URL
# or attach programmatically:
curl -s -X POST "http://$MANZANASD_ADDR/v0/streams" \
  -d '{"lease_id":"'$MANZANAS_LEASE'","format":"mjpeg","max_dim":600,"quality":60}'
# → mjpeg_url (multipart), url (WS, one binary JPEG per message), view_url
```

Streams need no lease to *view*, are capped per daemon, and keep
capturing `--stream-linger` after the last viewer leaves. A pool sim
with an open stream is never re-parked mid-capture.

## 5. Snapshot / restore deterministic state

Snapshots require the sim **shutdown** (`409 target_busy` otherwise):

```sh
manzanas shutdown <UDID> --lease $MANZANAS_LEASE
manzanas state snapshot --label before-onboarding     # ~6 s
manzanas boot <UDID> --lease $MANZANAS_LEASE
# ... mutate app state ...
manzanas shutdown <UDID> --lease $MANZANAS_LEASE
manzanas state restore snp_xxxxxxxxxxxx               # ~6 s
manzanas boot <UDID> --lease $MANZANAS_LEASE
```

Budget ~30-60 s per snapshot/restore cycle including the shutdown/boot
on either side. For "reset between test cases" prefer one snapshot after
first-launch setup, then restore per case; for "clean device at the end"
prefer the lease's `--reset erase` and let the daemon do it.

## 6. Journal: evidence comes for free

Every lease, action, screenshot, and state op is journaled per run
(run id = lease id), with screenshot artifacts stored server-side:

```sh
manzanas journal tail $MANZANAS_LEASE            # raw entries (--follow to live-tail)
manzanas journal export $MANZANAS_LEASE -o evidence.md   # PR-ready markdown
# equivalent over raw HTTP:
curl -s "http://$MANZANASD_ADDR/v0/journal/$MANZANAS_LEASE/export.md" -o evidence.md
```

`export.md` is PR-comment-ready: metadata table, per-action rows with
status and params, artifact digests. Attach it (plus the JPEGs you saved)
to the QA report instead of hand-writing a step log.

### QA evidence durability (required for issues and PRs)

A QA session's box — and every screenshot on it — is gone shortly after
the session ends. Evidence cited in a GitHub issue, PR, or FIXED/closing
verification comment MUST therefore be durable. Exactly two forms are
acceptable:

1. **Journal artifacts under a run id.** Upload the files to the run's
   journal while the lease is still active, then cite the run id plus
   artifact name/path (e.g. "run `lse_ab12cd34`, artifact
   `artifacts/deadbeef….png`"):

   ```sh
   manzanas journal upload $MANZANAS_LEASE 104-rm-today.png 105-rm-today2.png
   # or raw HTTP:
   curl -s -X POST "http://$MANZANASD_ADDR/v0/journal/$MANZANAS_LEASE/artifacts?name=104-rm-today.png&kind=screenshot" \
     --data-binary @104-rm-today.png
   ```

2. **Attached/embedded directly in the GitHub issue or PR** (drag-drop
   upload, or a `gh`-uploaded asset), so the image lives on GitHub.

Bare session-local filenames ("see `104-rm-today.png` from the QA
session") are **not** acceptable evidence: once the session's VM is
recycled the verdict can't be audited and a disputed retest has nothing
to compare against.

Rules of thumb:

- Any issue whose severity or verdict rests on visual evidence must
  attach or upload the key screenshots at filing time.
- Same for FIXED/closing verification comments: attach or link the pass
  evidence for at least the headline assertion.
- When the daemon is in the loop, prefer the journal path — screenshot
  actions are journaled automatically, so evidence collection is a side
  effect, not a manual step. Add `export.md` for the step log.
- Upload before releasing the lease: a run's journal becomes immutable
  once its lease is released or expires.
- Journal retention is bounded (default 7 days / 2 GiB — see
  [journal.md](journal.md)); for anything that must outlive that, also
  attach the images to the issue.

## 7. Release

```sh
manzanas lease release $MANZANAS_LEASE
```

With `--reset erase` on the acquire, release triggers erase → (pool
sims) re-slim → re-park automatically; `409 target_busy` on the next
acquire means the reset is still running. Release is idempotent.

## Known sharp edges (v0.2)

- `manzanas` CLI wants positional args **before** flags
  (`manzanas app install /path --lease X`, not `--lease X /path`).
- No `openurl`/deep-link action yet; `app launch` takes only a bundle id.
- Warm (simbridge) actions need the helper binary on the Mac; without it
  everything silently uses the ~1 s/action cold AXe path. simbridge
  currently fails to build on Intel Macs with a Swift-toolchain/SDK
  mismatch — Apple-Silicon hosts are the fast path.
- If the daemon port is already taken, older builds logged "listening"
  and then exited after pool adoption had already begun; current builds
  bind the port first and fail fast.

## Pitfalls: driving animated apps

Pitfalls found while dogfooding manzanasd against real React Native apps
on the simulator fleet, and the sequences that avoid them.

### Screenshot immediately after `type` races the re-render

A `screenshot` dispatched right after a `type` (or `tap`) frequently
captures the *pre-render* frame: AXe returns from the HID event as soon
as the keystrokes are delivered, while React Native still has to run the
JS `onChangeText`, re-render, and commit to the native view tree. The
capture is a valid screenshot of a stale UI, so the failure looks like
"my text never got typed" rather than a timing bug — and it reproduces
far more often on the Intel boxes, where a commit can take hundreds of
milliseconds.

Do not fix this with a fixed `sleep`. Instead, gate the screenshot on
the state you expect:

```jsonc
// POST /v0/actions:batch
{"lease_id": "lse_…", "actions": [
  {"kind": "type_into_element", "payload": {"label": "Email", "text": "a@b.co"}},
  {"kind": "wait_for_element",  "payload": {"value": "a@b.co", "timeout_ms": 5000}},
  {"kind": "screenshot",        "payload": {"format": "jpeg", "max_dim": 800}}
]}
```

`wait_for_element` on the typed value (or on whatever the input unlocks —
an enabled "Continue" button, a validation message) settles the race
deterministically. On a screen with no such marker, `wait_tree_stable`
with a short `timeout_ms` (1000–2000) is the next best gate; if even that
is unavailable, a small explicit settle delay is the last resort and
should be commented as such.

The same ordering applies to `observe`: the tree read right after a HID
action can predate the re-render.

### `wait_tree_stable` never settles on animated screens

`wait_tree_stable` waits for the a11y tree hash to repeat for N
consecutive polls. A screen with a looping animation (RN onboarding
carousels, Lottie/Reanimated loops, spinners, autoplaying video) mutates
the tree forever, so the streak never happens.

That is not an error: `timeout_ms` is a **max wait**, and when it elapses
the action returns `200` with `stable:false` plus the last observed
`hash`, `samples`, and `settled_ms`. Treat `stable:false` as "the UI is
live", not as a broken poll — real failures (AXe missing, target not
booted) still come back as action errors.

Guidance:

- On animated screens, prefer `wait_for_element` / `tap_element` /
  `type_into_element`: wait for the element you actually need.
- Use `wait_tree_stable` for load/transition settling (navigation, list
  fetch), where the tree does eventually hold still.
- Pass `require_stable: true` only when a non-settled tree must fail the
  step (e.g. capturing a reproducible tree hash in an eval); it restores
  the `408 timeout` error.

### Batch entries are strict about their keys

A batch entry accepts exactly `kind` and `payload`. Sending `params`
(the most common typo) is rejected with `400 bad_request` naming the
field, and no action in the batch runs — previously the misnamed object
was dropped and the action ran with its defaults, which silently turned
`tap {x, y}` into a bad-request-per-entry or a default-coordinate tap.
