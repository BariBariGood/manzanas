---
name: run-evals
description: Run the manzanas-eval scenario benchmark harness against a daemon and read its determinism report. Use when validating daemon changes end-to-end, measuring action latency, or checking reset determinism.
---

# Run the eval harness

`eval/` is a pure protocol client: it runs YAML scenarios against any daemon
N times (fresh lease + reset per run) and reports pass/fail, determinism
rate, flaky steps, and per-step latency percentiles.

## Build and run

```sh
go build -o bin/manzanas-eval ./eval/cmd/manzanas-eval

bin/manzanas-eval \
  --daemon http://<mac-host>:7433 \
  --runs 3 \
  --out eval-out \
  eval/scenarios/*.yaml
```

Outputs: `eval-out/report.md` (human), `report.json` (machine),
`<scenario>-<name>-run<N>.png` screenshot artifacts. Exit code is non-zero on
any failing run or cross-run tree-hash drift, so it drops straight into CI.

## Requirements & caveats (verified)

- The shipped scenarios need a **real Mac daemon**: they use `reset: erase`
  and dispatch `launch_app`/`observe`. Against a `--mock` daemon the acquire
  fails with `501 not_implemented: reset is not implemented in this build` —
  that is expected, not a harness bug. For Linux/CI coverage of the harness
  itself use `go test ./eval/...` (httptest fake daemon).
- Each run acquires a fresh lease with the scenario's reset policy, so eval
  runs erase their target on release — point evals at a host/label where that
  is acceptable, and prefer a device-specific label (e.g. `iphone-air`) so
  the reset can't select another agent's simulator.
- The runner renews the lease during long runs and deletes any snapshots the
  run created at teardown.
- Boots pass the daemon's safety gates; on a loaded host expect
  `503 overloaded` retries to stretch run times.

## Writing a scenario

YAML (strict parsing — unknown fields rejected; step names unique):

```yaml
name: my-check
lease:
  labels: [iphone-air]     # device-specific label on shared hosts
  reset: erase             # none | erase | snapshot:<name>
  ttl_seconds: 600
default_timeout: 120s
steps:
  - op: boot
    timeout: 300s
  - op: action
    kind: launch_app
    payload: {bundle_id: com.apple.Preferences, terminate_running: true}
  - op: assert
    assert:
      element_exists: {label: General}
```

Step ops: `boot`, `shutdown`, `action`, `fixture`, `snapshot`, `restore`,
`erase`, `wait`, `assert`. Assertions (one per assert step, polled every 2s
until timeout): `element_exists`, `element_absent` (precede with a positive
assertion — it passes as soon as nothing matches), `tree_hash`
(`save_as`/`equals_saved`/`equals`), `screenshot: {save: name}`.

Shipped references: `eval/scenarios/settings-navigation.yaml`,
`statusbar-fixture.yaml`, `snapshot-reset-determinism.yaml`. Full docs:
`docs/eval.md`.
