# Runs — the one-call run primitive and the YAML run-spec

A **run** executes the whole canonical agent loop from a single request:

```
acquire lease → boot → fixtures → install app → launch → steps →
artifact capture → release lease (applying its reset)
```

Instead of hand-sequencing `lease acquire`, `boot`, `app install`, a dozen
actions, and `lease release` (and remembering to release on every failure
path), you declare the run once and the daemon owns the choreography. All
evidence lands in the run journal exactly as if you had made the calls
yourself — the journal run ID **is** the run's lease ID — and the result
includes the journal's PR-ready markdown export.

Three frontends, one schema:

- `POST /v0/runs` — the wire API ([PROTOCOL.md §9](../proto/PROTOCOL.md)).
- `manzanas run spec.yaml` — the CLI.
- The MCP `run` tool (plus `run_status` for async polling).

All three also work pointed at a `manzanas-broker`: the broker places the
run on a fleet host with the same warm-first ranking as lease scheduling
and proxies it there ([broker.md](broker.md), "Federated runs").

## Quick start

```yaml
# login-smoke.yaml
name: login-smoke
target:
  labels: [ios26]
app:
  path: /Users/ci/builds/MyApp.app   # .app bundle on the daemon host
  bundle_id: com.example.myapp
steps:
  - action: tap_element
    with: {id: username}
  - action: type_into_element
    with: {id: username, text: agent}
  - action: type_into_element
    with: {id: password, text: hunter2}
  - action: tap_element
    with: {label: "Sign In"}
  - action: wait_for_element
    with: {label: "Welcome, agent!", timeout_ms: 5000}
  - name: quality gate
    action: audit
```

```console
$ manzanas run login-smoke.yaml -o evidence.md
run run_1a2b3c4d5e6f7a8b: passed
  journal run: lse_0123456789abcdef
  target: MOCK-UDID-1
  step 0 tap_element: ok
  ...
```

`-o` writes the journal's markdown export (the same document as
`GET /v0/journal/{run}/export.md`) — paste it into a PR comment.

Sync is the default: the command returns when the run finishes, bounded by
`timeouts.run_seconds`. For long runs pass `--async` and poll with
`manzanas run --status RUN_ID`; `manzanas run --ls` lists retained runs.

## Run-spec schema

Top level: `name`, `target` (required), `app`, `steps`, `artifacts`,
`timeouts`. Unknown fields are rejected.

### `target` — what to lease and how to prepare it

| field | meaning |
| --- | --- |
| `labels` | labels the target must carry (e.g. `[ios26, iphone-17-pro]`) |
| `udid` | pin a specific target |
| `runtime` | human-readable runtime requirement, e.g. `iOS 26.5` — translated to the derived label slug (`ios26.5`) |
| `device_type` | e.g. `iPhone 17 Pro` → `iphone-17-pro` |
| `image` | **reserved** — golden-image requirement (see below) |
| `fixtures` | list of `{name, payload}` state fixtures applied after boot ([state.md](state.md)) |
| `reset` | lease auto-reset applied at release: `none` (default), `erase`, `snapshot:<name>` |

At least one of `labels`, `udid`, `runtime`, or `device_type` is required.
Matching, queueing (waiting for a busy target up to
`timeouts.acquire_seconds`), and the warm pool's boot gating are the
normal lease/boot paths — a run behaves exactly like a well-behaved agent.

### `app` — install and launch

| field | meaning |
| --- | --- |
| `path` | `.app` bundle **on the daemon host** to install |
| `bundle_id` | app to launch (required to launch) |
| `launch` | default `true` when `bundle_id` is set; `false` = install only |
| `terminate_running` | relaunch over a running instance |
| `args` | extra launch arguments |

To run against a pre-installed app, give only `bundle_id`. To ship a build
to the daemon host first, copy it out-of-band (e.g. the CI artifact path
the daemon can see); uploading an app bundle through the API is not yet
supported.

### `steps` — the native step DSL

Each step is one action from the action surface
([PROTOCOL.md §5](../proto/PROTOCOL.md)), dispatched and journaled exactly
like `POST /v0/actions`:

```yaml
steps:
  - name: optional human label
    action: tap_element            # any action kind: tap, swipe, type,
    with: {id: username}           #   tap_element, type_into_element,
    timeout_seconds: 15            #   wait_for_element, scroll_to_element,
    continue_on_error: false       #   observe, screenshot, audit, ...
```

- `with` is the action payload, passed through verbatim.
- `timeout_seconds` bounds the step (default `timeouts.step_seconds`).
- Steps stop at the first failure; later steps report `skipped`. A step
  with `continue_on_error: true` runs even after an earlier failure and
  its own failure doesn't halt the run — but any failed step makes the
  run `failed`.
- `action: batch` runs `with.actions` (a list of `{kind, payload}`)
  through the batch endpoint in one step; `with.stop_on_error` defaults
  to `true`.

### `artifacts`

| field | meaning |
| --- | --- |
| `final_screenshot` | capture a screenshot into the journal after the last step, even on failure (default `true`) |
| `export` | include the journal's markdown export in the finished run as `export_md` (default `true`) |

Per-step evidence (screenshots, audit findings + annotated screenshots,
a11y tree hashes, correlated logs) is journaled by the action pipeline
regardless — these switches only control the run-level extras.

### `timeouts`

| field | default | meaning |
| --- | --- | --- |
| `acquire_seconds` | 60 | max wait for a queued lease |
| `run_seconds` | 600 (max 3480) | budget for the whole run |
| `step_seconds` | 60 | per-step budget when the step has none |

The run's lease TTL is derived from `run_seconds` (plus margin), so a
lease never expires mid-run; the run releases it the moment it finishes.

## Failure semantics

The lease is **always released** — with its reset — when the run ends,
whatever failed: a failing step, a stage error (boot, fixture, install,
launch), the run budget expiring, or an abandoned queued acquire. The
journal export is still attached to failed runs, so the evidence trail of
a red run is as complete as a green one. The run's `stage` field says how
far it got; the first failure is mirrored in `error`.

Run resources follow the v0 posture (tailnet-only, no auth): `GET
/v0/runs` and `GET /v0/runs/{id}` serve the full run — spec, lease ID,
and journal export — to any caller, like the lease and journal APIs.
Scoping runs to their `agent_id` lands with the auth slice.

## Reserved schema (documented, not yet implemented)

Both fields parse today (so specs are forward-compatible) but are refused
with `501 not_implemented`:

- **`target.image`** — declaring a golden image ([images.md](images.md))
  the target must be stamped from. Until then: stamp sims from the image
  first and select them by label/udid.
- **`steps[].maestro_flow`** — embedding a [Maestro](https://maestro.dev)
  flow file as a step. The adoption research recommends this as a cheap
  compatibility hook (shelling out to `maestro test` against the leased
  sim's UDID) rather than reimplementing Maestro's YAML; it needs Maestro
  and a JVM on the daemon host, so it ships as an opt-in follow-up. The
  schema keeps `action` and `maestro_flow` mutually exclusive per step so
  the hook can land without breaking existing specs.

## MCP

The `run` tool takes the spec as YAML (`spec_yaml`), an `agent_id` for
attribution, and optional `async`; `run_status` polls async runs. See
[mcp.md](mcp.md).
