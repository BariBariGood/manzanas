# State engine — snapshots, fixtures, per-lease auto-reset

`internal/state` implements `state.Engine`: deterministic environment
control for simulators. Everything is exposed over the protocol
(`/v0/state/...`, WS `state.*` — see [proto/PROTOCOL.md](../proto/PROTOCOL.md) §7)
and every mutating op is guarded by an active lease: the server derives the
target UDID from the lease, so an agent can never touch a sim it doesn't
hold.

## Snapshots

A snapshot captures a **shutdown** simulator's full state (installed apps,
data, settings, keychain, TCC grants).

### Mechanism: simctl clone + data-directory swap

- **Snapshot** = `xcrun simctl clone <udid> manzanasd-snap-<id>`. The clone
  is a full CoreSimulator device registered with the device set; on APFS
  the clone is a copy-on-write clonefile copy, so it is near-instant and
  shares blocks with the source. The registry hides devices named
  `manzanasd-snap-*` from target enumeration so snapshots are never leased.
- **Restore** = shut the target down (refused unless the caller asks for
  the reboot flow, see below), then replace
  `~/Library/Developer/CoreSimulator/Devices/<udid>/data` with a `cp -c -R`
  (clonefile) copy of the clone's data directory, preserving the target's
  UDID and device.plist identity — leases, labels, and client references
  stay valid.

### Tradeoff: clone vs. raw data-directory copy

Two candidate mechanisms were considered:

| | `simctl clone` (chosen for capture) | raw `cp` of the data dir (used for restore-in-place) |
|---|---|---|
| capture speed / space | APFS clonefile, near-instant, block-shared | same when `cp -c` works, full copy otherwise |
| integrity | CoreSimulator validates & registers the clone; survives runtime/Xcode version bookkeeping | bypasses CoreSimulator; safe only while the device is shutdown |
| restore-in-place | not supported — clone always creates a *new* UDID | swaps state under the *same* UDID |
| visibility | clone appears in `simctl list` (must be filtered) | invisible |

Neither alone is sufficient: clone can't restore in place (a new UDID would
break active leases and every client holding the old one), and a raw copy
as the capture format has no simctl-validated registration. So the engine
uses **clone for capture** (robust, instant, simctl-managed) and a
**data-dir clonefile swap for restore** (performed only while both devices
are shutdown, which is the same window in which `simctl erase` mutates the
directory). The `device.plist`/UDID of the target is never touched.

### Index

Snapshots are tracked in a JSON index (default
`~/.manzanasd/snapshots.json`, `--state-dir`/`MANZANASD_STATE_DIR`):

```json
{"snapshots": [{"id": "snp_a1b2c3", "source_udid": "...", "clone_udid": "...",
                "label": "clean", "created_at": "2026-08-01T00:00:00Z"}]}
```

Restore accepts a snapshot **ID** or a **label**; labels resolve to the most
recent snapshot with that label taken *from the same source target*
(restoring a snapshot onto a different UDID is refused).

## Booted-target guardrail

`snapshot`, `erase`, and `restore` require a shutdown target. If the target
is booted:

- snapshot/erase fail with `409 {"code":"target_busy"}`;
- restore fails with `target_busy` **unless** the request sets
  `"reboot": true`, in which case the engine performs the explicit
  shutdown → restore → boot cycle and reports `"rebooted": true` in the
  response.

## Fixtures

`POST /v0/state/fixtures` `{"lease_id","name","payload"}`. Each fixture is
its own file behind the `Fixture` interface, wired in
`internal/state/fixture.go`:

| name | payload | simctl |
|---|---|---|
| `statusbar` | `{"time":"9:41","batteryLevel":100,...}` or `{"clear":true}` (keys map 1:1 to flags) | `status_bar override/clear` |
| `privacy` | `{"action":"grant\|revoke\|reset","service":"photos","bundle_id":"..."}` | `privacy` |
| `push` | `{"bundle_id":"...","payload":{"aps":{...}}}` | `push ... -` (stdin) |
| `locale` | `{"language":"zh-Hans"\|[...], "locale":"zh_CN"}` | `spawn ... defaults write .GlobalPreferences` |
| `timezone` | `{"tz":"America/Los_Angeles"}` | `spawn ... launchctl setenv TZ` (affects subsequently launched processes) |
| `url` | `{"url":"myapp://..."}` | `openurl` |

Most fixtures require a booted target; simctl's error is propagated
verbatim when it isn't.

## Per-lease auto-reset

`POST /v0/leases` accepts `"reset": "none" | "erase" | "snapshot:<name>"`.
When such a lease ends (release **or** TTL expiry), the lease manager holds
the target (it cannot be acquired or promoted to a queued lease), runs the
reset through the state engine — shutdown if needed, then `simctl erase` or
the snapshot data-dir swap, leaving the sim shutdown — and only then frees
the target for the next holder. The next agent always gets a clean,
shutdown sim. A `snapshot:<name>` reset whose snapshot can't be resolved
when the reset runs (typo, or the snapshot/backing clone was deleted
mid-lease) degrades to an `erase` — the next holder still gets a clean
target. If the reset fails for any other reason (simctl error, copy
failure), the target stays quarantined — held by the reset sentinel, never
handed out dirty — until an operator resolves the underlying problem and
frees it with `POST /v0/targets/{udid}/clear-quarantine`; the failure is
logged by the daemon.
