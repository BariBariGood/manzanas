---
name: run-the-daemon
description: Run or restart the manzanasd daemon - flags, warm pool and safety gates, journal settings, 503 overloaded handling, and install/upgrade via deploy/install.sh. Use when starting a daemon on a Mac host, tuning its gates, or debugging boot refusals.
---

# Run the manzanasd daemon

One daemon per Mac, port `:7433`. Never run two daemons against the same
CoreSimulator state; a second experimental daemon goes on `7434+` with
`--mock` only.

## Start

```sh
make build                                 # bin/manzanasd, bin/manzanas, bin/manzanas-broker
./bin/manzanasd --addr :7433                # on a Mac with Xcode
./bin/manzanasd --addr :7433 --mock         # anywhere (Linux/CI): mock fleet, no simctl
curl -s localhost:7433/v0/healthz          # {"ok":true,"version":"v0"}
```

Production installs use the LaunchAgent: `deploy/install.sh --binary
../manzanasd` (installs to `~/bin/manzanasd`, RunAtLoad+KeepAlive, logs to
`~/.manzanasd/logs/`, log rotation). Upgrade = re-run install.sh with the new
binary. Remove with `deploy/uninstall.sh [--purge]`.

## Flags that matter (each also has an `MANZANASD_*` env var)

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:7433` | listen address |
| `--mock` | off | mock registry (no simctl; state/actions return 501) |
| `--pool-sims` | empty | comma-separated UDIDs kept in the park/thaw warm pool |
| `--pool-slim-profile` | empty | simslim profile applied to pool sims before parking (simslim v0.6.0+: batched launchctl transitions make the slim step much faster, esp. Intel; `simslim verify` gives exact drift checks; profiles needing in-app purchases keep `store` in their `except` array) |
| `--pool-max-running` | 0 = class default (2 Intel / 4 Apple Silicon) | cap on Booted un-parked sims; negative disables |
| `--pool-load-factor` | 0 = 2.0 | refuse boots when 1-min load > factor x cores; negative disables |
| `--pool-min-free-disk-gb` | 0 = 5 | refuse boots below this free disk; negative disables |
| `--pool-footprint-cap-mb` | 0 = 3072 | watchdog recycles a pool sim above this phys_footprint; negative disables |
| `--simbridge` | `~/bin/simbridge` or `$PATH` | warm-actions helper binary |
| `--no-warm` | off | disable the warm actions backend |
| `--warm-max-targets` / `--warm-idle-ttl` | 4 / 5m | resident helper capacity / idle shutdown |
| `--janitor-reap-stale-locks` | off | let the janitor shut down (never delete) unmanaged Booted sims whose `sim-<udid>` fleet lock is stale (>=2h) or missing, after the staleness persists a full grace (>=10 min); recommended ON for fleet daemons |
| `--lock-dir` | `/tmp/manzanas_locks` (env `MANZANAS_LOCK_DIR`) | fleet lock directory holding `sim-<udid>` lock files |
| `--journal-dir` | `~/.manzanasd/journal` | empty string disables the journal |
| `--journal-max-age` / `--journal-max-bytes` | 168h / 2 GiB | journal GC bounds |
| `--stream-max-streams` / `--stream-max-viewers` / `--stream-max-fps` / `--stream-linger` | see `--help` | streaming limits |

## Safety gates & 503 handling

On real (darwin, non-mock) daemons every boot passes: running-sim cap, load
gate, free-disk gate. A refusal is `503 {"code":"overloaded"}` — this is
back-pressure, not an error: clients back off and retry (or lease on another
host via the broker). Don't loosen the gates on shared boxes to make a task
pass; parked pool sims don't count against the cap.

A boot may also surface `500` carrying the failure of a *previous* accepted
boot that died in the background — just retry.

## Warm pool

`--pool-sims` sims are booted, slimmed (`--pool-slim-profile`), then PARKED
(SIGSTOPped, ~0 CPU). Lease grant thaws in ~25ms (AS) / ~225ms (Intel);
release+reset erases, re-boots, re-parks. See the park-thaw-semantics skill
before touching parked sims by hand.

## Debugging

- Logs: `~/.manzanasd/logs/manzanasd.{out,err}.log` (LaunchAgent installs).
- `501 not_implemented` from state/actions endpoints = mock build or missing
  backend (e.g. no simctl on the host, journal disabled).
- `503 unavailable` from actions = AXe missing or the a11y bridge stayed
  unready — check `~/bin/axe` exists and the sim is fully booted.
- No auth in v0: tailnet-only. Never expose the port publicly.
