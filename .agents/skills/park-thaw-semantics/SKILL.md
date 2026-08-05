---
name: park-thaw-semantics
description: Rules for parked (SIGSTOPped) warm-pool simulators - why simctl/AXe hang on them, the thaw-before-shutdown invariant, and how the pool cycles sims. Read before running any manual simctl/ps/kill against a host whose manzanasd daemon has a warm pool.
---

# Park/thaw semantics

The warm pool (`internal/warm`) keeps pool sims booted+slimmed but PARKED:
the whole process tree from `launchd_sim` down is SIGSTOPped, so an idle warm
sim costs ~0 CPU. On lease grant the tree is SIGCONTed (thawed) using the PID
list cached at park time (~25ms Apple Silicon / ~225ms Intel). On
release+reset the sim is erased, re-booted, and parked again.

## Hard invariants — violating these wedges the host

1. **ALWAYS thaw before shutdown.** `xcrun simctl shutdown` on a parked sim
   wedges for ~34s. The daemon does this for you; never `simctl shutdown` a
   pool sim by hand.
2. **Never route actions at a parked sim.** While parked, AXe fails and
   `simctl spawn` hangs. The daemon thaws on lease grant — acquire a lease
   instead of poking the sim directly.
3. **Never SIGKILL a parked tree.** If a parked sim looks stuck, restart via
   the daemon (it thaws everything on shutdown — `ThawAll` — so trees are
   never left stopped behind a dead daemon).
4. Double STOP / double CONT are harmless; Park/Thaw are idempotent.

## How to recognize a parked sim

- `xcrun simctl list devices` shows it `Booted` but it responds to nothing.
- `ps -axww -o pid,ppid,state,command | grep <UDID>` shows state `T`
  (stopped) on the tree.

If you find a stopped tree and the daemon is NOT running (crashed without
cleanup): `kill -CONT` each PID of the tree, then `simctl shutdown`. Only do
this when you have verified no daemon owns the sim.

## Related machinery (same package)

- **Footprint watchdog**: sweeps pool sims (default every 1m); a sim whose
  tree phys_footprint exceeds `--pool-footprint-cap-mb` (default 3 GiB) is
  recycled (erase→boot→park). Footprint is measured with `/usr/bin/footprint`
  (kernel phys_footprint) — summing ps RSS overcounts ~8x on sim trees.
- **Orphan reaper**: kills leftover `launchd_sim` trees the daemon itself
  parked earlier (ownership ledger keyed by PID+command, never a recycled
  PID). It does not touch trees it never owned.
- **Quarantine**: if a post-lease reset fails or hangs (>10 min bound), the
  target is quarantined rather than handed out dirty. Clear with
  `POST /v0/targets/{udid}/clear-quarantine` after fixing the cause.
