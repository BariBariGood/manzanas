# Warm actions backend (simbridge)

The cold action path shells out to the AXe CLI once per action. Every
invocation pays process spawn + FBSimulatorControl bootstrap (private
framework loading, CoreSimulator set construction, HID/a11y connection):
about **0.9–1.4s per tap or describe-ui** on an Intel Mac even with the
simulator hot. Agents issue actions in tight loops, so that overhead
dominates end-to-end latency.

The warm backend removes it by keeping a **resident helper process per
simulator** — `simbridge` — that bootstraps once and then services
actions over stdin/stdout JSON lines in a few milliseconds each.

(A third backend serves mock targets: `manzanasd --mock` routes the same
handlers to an in-process synthetic app so the full action loop runs
off-Mac — see [mock.md](mock.md).)

```
POST /v0/actions ─► WarmBackend ──(warm kind + helper alive)──► Pool ─► simbridge --udid X   (resident)
                        │                                                 │ FBSimulatorControl, connected once
                        └──(cold-only kind, or any transport failure)──► AXeBackend ─► spawn `axe ...` per action
```

## Options considered

| Option | Verdict |
|---|---|
| **Resident Swift helper on FBSimulatorControl** (`helpers/simbridge/`) | **Chosen.** One small Swift file, links the MIT-licensed FBSimulatorControl frameworks that already ship with every AXe install (no new dependency to distribute). Bootstraps once per simulator; each subsequent action is one JSON line. |
| AXe server/daemon mode | AXe has no public server mode. It does keep an internal per-UDID `hid-broker` daemon (60s idle) for *touch primitives only*, but every CLI invocation still spawns a process and loads frameworks, and describe-ui/a11y is not brokered. Speaking the broker's unix-socket protocol directly from Go was rejected: it is an undocumented internal protocol that covers taps only. |
| `idb_companion`-style bridge | facebook/idb is MIT but effectively unmaintained (broken against Xcode 16+); resurrecting it is far more surface than we need. |

Go stays the daemon language: the helper is deliberately dumb (one
simulator, one op at a time, no state), and all lifecycle policy lives in
Go where it is unit-testable on Linux.

## What runs warm

`tap`, `swipe`, `button`, `key`, `key_sequence`, and `observe`
(describe-ui, compacted by the same Go code as the cold path, including
the a11y-bridge retry loop). The volume buttons are not supported on
either path (AXe has no volume support); `button` accepts the same name
set warm and cold. HID kinds honor the `ax_hashes` evidence flag exactly
like the cold path (before/after tree hashes via the resident helper,
best-effort and time-bounded). The `wait_for_element` / `wait_tree_stable`
actions run their per-poll a11y reads through the resident helper too,
falling back to a cold poll on transport failure.

A `refresh: true` payload on `observe`, `wait_for_element`,
`wait_tree_stable`, `tap_element`, or `type_into_element` opts the a11y
reads out of the resident helper: the observe (or every poll) runs on a
freshly spawned AXe process instead. The helper's long-lived
FBSimulatorControl accessibility connection can occasionally serve a
stale snapshot (a frontmost modal/sheet missing from the tree while the
background screen still shows); a fresh process opens a fresh connection,
which cannot. It costs the full cold spawn per read, so use it as an
escape hatch when the tree looks stale, not as a default. The flag is
additive and understood by the Go side only — older simbridge builds need
no changes. It is best-effort: on a host with no AXe CLI (helper-only),
or when no warm helper is configured (the cold path is always fresh),
the flag is a no-op rather than an error.

Everything else — `type` (AXe's text→HID keymap is nontrivial and typing
is rare), `screenshot`, and the `simctl`-backed app lifecycle ops — stays
on the cold path. The cold backend is untouched; the warm backend wraps
it.

## Lifecycle

- **Spin-up on first action**: the pool spawns `simbridge --udid <UDID>`
  on the first warm action for a target and waits for its `ready`
  handshake (30s budget).
- **Idle TTL**: helpers idle for `--warm-idle-ttl` (default 5m) are shut
  down by a janitor loop.
- **Capacity**: at most `--warm-max-targets` (default 4) simulators stay
  warm; the least-recently-used idle helper is evicted for a new target.
- **Health / crash recovery**: any transport failure (helper crashed,
  wedged, malformed frame) closes the helper and retries the action once
  on a freshly spawned one; if that also fails, the action transparently
  falls back to the cold AXe path and the slot is freed.
- **Op timeout**: each helper call is bounded (30s default); a helper
  that blows it is treated as wedged — closed, evicted, and the action
  falls back cold — so one stuck process cannot stall its simulator.
  Worst-case user-visible latency is the op timeout plus the ~3s
  close-then-kill grace (~33s with defaults) before the fallback runs.
- **Spawn cooldown**: after a helper fails to start for a UDID — or dies
  twice in a row mid-call — warm attempts for that UDID are skipped for
  30s so actions go straight to the cold path instead of re-paying the
  failed bootstrap every call.
- **At-most-once inputs**: a transport failure after the request was
  written to the helper is marked *delivered* — the tap/key may already
  have landed. Non-idempotent ops are then neither replayed on a fresh
  helper nor re-run cold; the caller gets `unavailable`. Only
  never-written failures (and idempotent reads like observe) retry.
- **Reset invalidation**: a lease-end auto-reset (erase/reboot) drops
  the target's resident helper (`WarmBackend.Invalidate`) before the
  reset runs, and the helper itself exits when its simulator leaves the
  booted state, so no action is ever served over a connection to
  pre-reset device state.
- **Transparent fallback**: when the simbridge binary is missing, spawn
  fails, or a helper dies mid-call, actions are served by the cold
  backend. Warm is purely an accelerator; behavior and payload schemas
  are identical.

Daemon flags: `--simbridge` / `MANZANASD_SIMBRIDGE` (binary path; default
`~/bin/simbridge` or `$PATH`), `--no-warm` / `MANZANASD_NO_WARM`,
`--warm-max-targets`, `--warm-idle-ttl`.

## Wire protocol (v1)

Line-delimited JSON on stdin/stdout; one response line per request line,
in order. On startup the helper emits a handshake once frameworks are
loaded and the simulator connection is up:

```json
{"event":"ready","udid":"<UDID>","version":1}
{"id":"1","op":"tap","args":{"x":100,"y":200}}
{"id":"1","ok":true,"result":{"x":100,"y":200}}
{"id":"2","op":"describe_ui"}
{"id":"2","ok":true,"result":{"raw":"[ ...accessibility tree JSON... ]"}}
{"id":"3","op":"button","args":{"name":"homer"}}
{"id":"3","ok":false,"code":"bad_request","error":"unknown button"}
```

Ops: `ping`, `tap`, `swipe`, `button`, `key`, `key_sequence`,
`describe_ui`. The schema is owned by `helpers/simbridge`; the Go side
treats results as opaque maps.

## Building simbridge

CI cannot build the helper (macOS + Xcode + AXe frameworks required), so
it is built on a Mac and distributed as a prebuilt binary:

```sh
# on a Mac with Xcode and AXe installed (frameworks at ~/axe/Frameworks):
helpers/simbridge/build.sh ~/bin/simbridge
# or against another AXe install:
AXE_FRAMEWORKS=/opt/axe/Frameworks helpers/simbridge/build.sh
```

The binary hard-links its rpath to the frameworks directory it was built
against, so build it on (or for) the host that runs it.

Caveat: prebuilt AXe frameworks embed compiled `.swiftmodule`s tied to the
exact Swift compiler that produced them; with any other compiler swiftc
falls back to the shipped `.swiftinterface`, which fails to parse (the
`FBSimulatorControl` class shadows its own module name, so qualified
references like `FBSimulatorControl.FBSimulatorContentSizeCategory`
resolve against the class). This is a **toolchain-version problem, not an
architecture problem** — the same Swift source builds and runs fine on
x86_64. Concretely: AXe v1.8.0 frameworks were compiled with Swift 6.3.2,
so they compile as-is on a 6.3.2 host (the M3) but fail on the Intel
boxes' Xcode 26.6 / Swift 6.3.3. At **runtime** the prebuilt frameworks
are fine on any toolchain (dyld never reads swiftmodules), which is why
the AXe CLI itself works everywhere.

When the toolchain doesn't match, rebuild the frameworks locally first
(wrapped by `build-frameworks.sh`; requires `brew install xcodegen`,
~15 min), then point the build at them:

```sh
helpers/simbridge/build-frameworks.sh          # → ~/.cache/manzanasd/axe-frameworks-<swift-ver>
AXE_FRAMEWORKS=~/.cache/manzanasd/axe-frameworks-<swift-ver> \
  helpers/simbridge/build.sh ~/bin/simbridge
```

To ship one binary to several same-arch hosts with different home
directories, build with a portable rpath and deploy the rebuilt
frameworks next to the binary on each host:

```sh
SIMBRIDGE_RPATH='@executable_path/../axe/FrameworksLocal' \
AXE_FRAMEWORKS=... helpers/simbridge/build.sh ~/bin/simbridge
# deploy: ~/bin/simbridge + the frameworks dir as ~/axe/FrameworksLocal
```

Release strategy: build on a fleet Mac per architecture and attach
`simbridge-darwin-amd64` / `simbridge-darwin-arm64` to GitHub releases
alongside the daemon binaries.

## Measured latency (Intel Mac, iPhone 17 / iOS 26.5 slim sim, 10 iterations through the daemon)

| action | cold (AXe spawn per call) | warm (resident simbridge) | speedup |
|---|---|---|---|
| tap | median 3.05s (2.92–3.20s) | median 30ms (27–373ms incl. first-call spawn) | ~100x |
| observe (describe-ui) | median 2.88s (2.69–3.22s) | median 2.43s (2.35–2.78s) | ~1.2x |

On Apple silicon (M3, same sim config) the cold spawn is much cheaper, so
the warm win is smaller but still large: cold tap ~1.0s vs warm ~32ms
(~30x); cold observe ~0.9s vs warm ~0.68s (~1.3x).

Tap latency is dominated by process spawn + framework bootstrap, which the
warm path eliminates entirely. Observe is dominated by the simulator-side
accessibility serialization itself (tree-size dependent: ~1.1s with
Settings frontmost vs ~2.4s on springboard), so warm only shaves the
bootstrap share off it.
