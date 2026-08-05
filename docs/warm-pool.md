# Warm pool — park/thaw + host safety gates

`internal/warm` keeps a pool of pre-slimmed simulators **parked**
(process tree SIGSTOPped) so a lease grant gets a ready sim in
milliseconds instead of a cold boot, while the same package's gates keep
the host from being melted by too many sims.

Why parking: a booted sim, even slimmed, still burns CPU idling —
measured **~213% host CPU across idle sims before parking vs ~1–6%
after** (a SIGSTOPped tree costs ~0 CPU and keeps its RAM). Thawing is a
cached-PID `SIGCONT`: **~26 ms on Apple Silicon (~225 ms Intel) vs a
5 s+ slim-sim boot** (15–30 s on cold Intel).

```
            lease granted            lease released
 parked ───────────────► booted ───────────────────► erase → boot → slim → park
 (SIGSTOP, ~0 CPU)  thaw  (agent drives it)   reset      (clean, parked again)
```

## Pool membership

- `--pool-sims` / `MANZANASD_POOL_SIMS` — comma-separated UDIDs the daemon
  parks at startup. Adds retry with capped backoff (a daemon restarting
  during a build races the load spike); unknown UDIDs are dropped.
- `--pool-slim-profile` / `MANZANASD_POOL_SLIM_PROFILE` — simslim profile
  applied before each park (empty = don't slim; `default` = simslim's
  built-in profile). Sims stamped from a slim golden image keep their
  recorded disables re-applied after every erase regardless of this flag
  (see [images.md](images.md)). simslim v0.6.0 batches its launchctl
  transitions, so the slim step of the release→re-park cycle is up to
  7.8× faster on Intel (3.4× on Apple silicon) — the cycle is dominated
  by erase+boot, but the win matters most on the Intel boxes. Pools that
  must exercise in-app purchases need a profile whose `except` array
  includes `store` (v0.6.0 keeps the AMS payment-sheet daemons enabled
  with `--except store`); see [images.md](images.md) before changing a
  shared fleet profile.
- Pool sims still appear in `/v0/targets` and are leased like any other
  target; a parked member reports `Booted` (its tree is alive, just
  suspended).

## Lifecycle

- **Thaw on grant**: the instant a lease on a pool member becomes active,
  the pool SIGCONTs its cached PID list. No client action is needed.
- **Re-park on release**: after the post-lease reset (erase → boot →
  slim), the member is parked again, clean, for the next holder. If only
  the park step fails on a clean Booted sim, the sim is still handed out;
  the watchdog re-parks it when idle.
- **Idle re-park**: the watchdog parks an un-leased, un-streamed member
  that was booted/thawed out-of-band, after a 15-minute wake grace (so an
  operator's manual boot isn't silently frozen).
- **Streams**: frame capture against a SIGSTOPped tree hangs, so
  `POST /v0/streams` refuses parked sims (`409 target_busy`) and the
  watchdog never parks a sim with an open stream. Lease it to thaw it.
- **Crash recovery**: on startup the daemon SIGCONTs any pool trees a
  previous daemon left frozen (`RecoverFrozenParks`); on shutdown
  `Pool.Close` thaws everything, so no tree is ever stranded stopped.

## Footprint watchdog

Fleet measurements saw a slim sim balloon from 1.9 GB to 17.9 GB in 14
minutes, so every pool member's `phys_footprint` is polled (default every
1 m, `--pool-watchdog-interval`) against a cap (default 3 GiB,
`--pool-footprint-cap-mb`; negative disables):

- **parked/idle** runaways are recycled immediately: shutdown → erase →
  boot → slim → park. Recycling reserves the target in the lease manager
  first, so it can never be granted mid-erase.
- **leased** runaways are only flagged (the post-lease reset cleans them).
- failed rebuilds back off per target (2–30 m) so an overloaded host
  isn't erased-and-booted every sweep.

## Host safety gates

The pool wraps the registry (`warm.Guard`), so **every** boot — pool or
not — passes three gates on real (darwin, non-mock) daemons. A refusal is
`503 {"code":"overloaded"}`: back off and retry.

| Gate | Default | Flag |
|---|---|---|
| Running-sim cap (Booted AND not parked) | capacity class: 2 Intel / 4 Apple Silicon | `--pool-max-running` |
| Load gate | refuse when 1-min load > 2.0 × cores | `--pool-load-factor` |
| Free-disk gate | refuse below 5 GiB free on the sim volume | `--pool-min-free-disk-gb` |

For all three: `0` = default, negative = disable. The capacity class also
caps parked members (6 Intel / 4 AS) and serializes concurrent boots
(1 Intel / 2 AS) — Intel boot storms drove 1-min load past 500.

The daemon enforces per-host what the lock protocol asks agents to
respect manually.

## Flags recap

```
--pool-sims UDID[,UDID...]      sims to park at startup      MANZANASD_POOL_SIMS
--pool-slim-profile NAME        simslim profile before park  MANZANASD_POOL_SLIM_PROFILE
--pool-footprint-cap-mb N       watchdog recycle threshold   MANZANASD_POOL_FOOTPRINT_CAP_MB
--pool-watchdog-interval DUR    watchdog sweep interval      MANZANASD_POOL_WATCHDOG_INTERVAL
--pool-max-running N            running-sim cap              MANZANASD_POOL_MAX_RUNNING
--pool-load-factor F            load gate                    MANZANASD_POOL_LOAD_FACTOR
--pool-min-free-disk-gb N       disk gate                    MANZANASD_POOL_MIN_FREE_DISK_GB
```

See also: [troubleshooting.md](troubleshooting.md) (503 overloaded,
parked-sim semantics), [actions-warm.md](actions-warm.md) (the *other*
"warm" — resident action helpers), [images.md](images.md) (slim golden
images the pool sims are usually stamped from).
