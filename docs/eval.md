# Eval harness — reproducible agent QA benchmark

`eval/` is a scenario-driven benchmark harness that proves manzanasd's
determinism value: it runs scripted protocol scenarios against a real
daemon N times (fresh lease + reset each run) and reports pass/fail,
determinism rate, flaky steps, and per-step latency percentiles.

The harness is a **pure protocol client** — it imports only `proto/` wire
types and talks HTTP to any daemon, so it runs from Linux against a remote
Mac daemon (or a `--mock` daemon in CI).

## Running

```sh
go build -o bin/manzanas-eval ./eval/cmd/manzanas-eval

bin/manzanas-eval \
  --daemon http://mac-host:7433 \
  --runs 3 \
  --out eval-out \
  eval/scenarios/*.yaml
```

Outputs in `--out`:

- `report.md` — human-readable summary (determinism rate per scenario,
  flaky steps, p50/p90/max latency per step, per-run detail).
- `report.json` — the same data, machine-readable.
- `<scenario>-<name>-run<N>.png` — screenshot artifacts from `screenshot`
  assertions.

Exit code is non-zero if any scenario has a failing run or if a
scenario's saved tree hashes drifted between runs (`hash_consistent:
false`), so the harness drops straight into CI.

Flags: `--daemon` (default `http://localhost:7433`), `--runs` (default 3),
`--out` (default `eval-out`), `--agent-id` (lease `agent_id`, default
`manzanas-eval`), `--udid` (pin every lease to one target, overriding any
scenario `lease.udid`), `--profile` (host-class timing profile, `m3` or
`intel`, default `m3`), `--timeout-scale` / `--wait-scale` (override the
profile's multipliers), `--overload-budget` (total back-off-and-retry time
for boots the daemon refuses with `503 overloaded`, default 2m; 0 = fail
fast).

### Host-class timing profiles

Scenario timeouts in this repo are tuned on the M3. On the Intel boxes
boot, observe, and snapshot/restore run roughly 2-4x slower, so
`--profile intel` scales all client-side budgets (step timeouts, the
scenario `default_timeout`, and `acquire_timeout`) by 3x and the polling
knobs of `wait_*` action payloads (`timeout_ms`, `interval_ms`, `for_ms`)
by 2x — a longer interval stops a tight poll from thrashing a slow
target where each observe is expensive. Counters like `stable_samples`
are never scaled.

### 503 overloaded backoff

On real daemons every boot passes host safety gates (running-sim cap,
load gate, free-disk gate — PROTOCOL.md §2); a refusal answers
`503 {"code":"overloaded"}`. The harness backs off (exponential, 2s base,
30s cap, with jitter so concurrent harnesses don't retry in lockstep) and
retries the boot until `--overload-budget` is exhausted.

## Scenario files

A scenario is YAML (or JSON — the decoder accepts both): a lease spec plus
an ordered list of steps. Parsing is strict: unknown or misspelled fields
are rejected. Step names must be unique within a scenario (reports
aggregate by step name). Each run acquires a fresh lease (with the
scenario's reset policy, so the *next* holder always gets a clean target),
executes the steps in order, and releases the lease. A failing step stops
the run (later steps depend on earlier ones); remaining runs still execute.
While a run is in flight the runner renews the lease periodically so long
scenarios never outlive the daemon's TTL, and any snapshots the run created
are deleted at teardown (they are backed by real simulator clones on the
host) before the lease is released. Teardown cross-checks the daemon's
snapshot listing against the scenario's snapshot labels, so a capture that
completed on the host after the client gave up is still cleaned up.

```yaml
name: settings-navigation
description: Launch Settings and verify the root list.
lease:
  labels: [simulator]      # lease label match; on shared hosts prefer a
                           # device-specific label so the lease (and its
                           # reset policy) can't select another agent's
                           # simulator
  # udid: AAAA-...         # optional — pin the lease to one target UDID
                           # (must also match labels, if any); at least
                           # one of labels/udid is required
  reset: erase             # "none" | "erase" | "snapshot:<name>"
  ttl_seconds: 600
  acquire_timeout: 5m      # max wait for a queued lease (default 5m)
default_timeout: 120s      # per-step timeout unless overridden
steps:
  - name: boot             # optional; defaults to "<NN>-<op>"
    op: boot
    timeout: 300s
  - op: action
    kind: launch_app
    payload: {bundle_id: com.apple.Preferences, terminate_running: true}
  - op: wait
    duration: 5s
  - op: assert
    assert:
      element_exists: {label: General}
```

### Step ops

| op | fields | semantics |
|---|---|---|
| `boot` | — | boot the leased target, wait until `Booted` |
| `shutdown` | — | shut down, wait until `Shutdown` |
| `action` | `kind`, `payload` | dispatch any protocol action (`tap`, `swipe`, `type`, `button`, `key`, `observe`, `screenshot`, `install_app`, `launch_app`, `terminate_app`, ...) |
| `fixture` | `fixture`, `fixture_payload` | apply a state fixture (`statusbar`, `privacy`, `push`, `locale`, `timezone`, `url`) |
| `snapshot` | `snapshot_label` | capture a snapshot of the (shutdown) target |
| `restore` | `snapshot`, `reboot` | restore a snapshot by ID or label |
| `erase` | — | factory-reset the (shutdown) target |
| `wait` | `duration` | fixed settle delay |
| `assert` | `assert` | check a condition (below) |

Every step takes an optional `timeout` (Go duration string); steps without
one use the scenario's `default_timeout` (default 60s). Exception: a
`wait` step without its own `timeout` is always given at least
`duration`+1s so the sleep fits its budget; an explicit `timeout` must
exceed the wait's `duration` (validated — an equal or shorter timeout
could never pass).

### Assertions

Exactly one per `assert` step. Assertions poll every 2s until they hold or
the step's timeout expires (UI trees settle asynchronously, and observe
backends can be transiently unavailable right after an app launch); the
last failure is reported. Note `element_absent` therefore passes as soon
as the element is missing — precede it with a positive assertion or a
`wait` if the UI needs time to reach the state you mean to check.

- `element_exists: {role?, label?, value?}` — run `observe` and require a
  matching node in the accessibility tree. `label`/`value` match as
  substrings, `role` exactly.
- `element_absent: {...}` — same query, require NO match.
- `tree_hash: {save_as: <name>}` — record the observe tree hash under a
  name; `tree_hash: {equals_saved: <name>}` — require the current hash to
  equal a recorded one; `tree_hash: {equals: <literal>}` — compare against
  a literal hash.
- `screenshot: {save: <name>}` — capture a screenshot, require non-empty
  PNG bytes, and write `<scenario>-<name>-run<N>.png` under `--out`.
  `<name>` must be a plain filename component (no path separators).

### Determinism measurement

Two signals in the report:

1. **Determinism rate** = passing runs / total runs per scenario. A
   scenario whose assertions encode "same starting state ⇒ same UI" (see
   `snapshot-reset-determinism`) passing 100% of runs is direct evidence
   the reset machinery delivers reproducible environments.
2. **Hash consistency** — every `tree_hash: {save_as: ...}` value is also
   compared *across* runs; the report flags a scenario whose saved hashes
   drift between passing runs.

Steps that pass in some runs and fail in others are listed as **flaky
steps**, separating scenario-level nondeterminism from a hard failure.

## Shipped scenarios (`eval/scenarios/`)

- `settings-navigation.yaml` — stock Settings app: launch, assert the root
  list, scroll, assert below-the-fold content, screenshots.
- `statusbar-fixture.yaml` — pin the status bar to 9:41/100%, assert the
  override is observable, clear it, assert it is gone.
- `snapshot-reset-determinism.yaml` — record a pristine tree hash, snapshot,
  dirty the UI, restore, and require the hash to match exactly (status bar
  pinned so time/battery can't perturb the hash).

## Package layout

- `scenario.go` — scenario/step/assertion schema, YAML/JSON loader,
  validation.
- `client.go` — minimal HTTP protocol client (leases, targets, actions,
  state). Self-contained by design; swap for a shared client package if
  one lands.
- `runner.go` — N-run executor, per-step timing, lease lifecycle.
- `ops.go` + `op_*.go` — one file per step op, wired via the `ops`
  registry.
- `tree.go` — accessibility-tree element matching.
- `report.go` — aggregation (percentiles, flaky steps, hash consistency)
  and markdown/JSON rendering.
- `cmd/manzanas-eval/` — the CLI entrypoint.

Unit tests run against an `httptest` fake daemon (`fakedaemon_test.go`),
so `go test ./eval/...` is Linux/CI safe.
