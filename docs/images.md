# Golden images — slim once, stamp out sims in seconds

`internal/state`'s `ImageStore` implements golden images: build a simulator
once (optionally slimmed with [simslim](https://github.com/MobAI-App/simslim)),
archive its data directory, and later **stamp** out N pre-slimmed, leasable
simulators from the archive in seconds — instead of paying the slim cost
for every sim. simslim v0.6.0 batches its launchctl transitions, making
the slim reconfigure itself much faster than before (up to 7.8× on
Intel, 3.4× on Apple silicon), but a stamp is still far cheaper than a
fresh slim per sim.

## Build

`POST /v0/images/build` `{"device_type","runtime","name?","slim_profile?"}`
→ `201 {ImageInfo}`.

`device_type` and `runtime` accept either display names (`"iPhone 17"`,
`"iOS 26.5"`) or CoreSimulator identifiers
(`com.apple.CoreSimulator.SimRuntime.iOS-26-5`): `simctl create` only
takes runtime identifiers, so the daemon resolves runtime display names
via `simctl list runtimes` first. An unknown runtime or device type is a
`400 bad_request` listing the installed runtimes.

A `slim_profile` is refused (`400 bad_request`) on iOS runtimes older
than 18: simslim silently no-ops there, which would archive an
unslimmed image labelled slim. As a second line of defense a slim that
leaves zero services disabled fails the build.

1. The daemon creates a **fresh** simulator named `manzanasd-img-<id>`
   (`simctl create`). The registry hides `manzanasd-img-*` devices, so the
   builder is never enumerated or leased.
2. If `slim_profile` is set, the builder is booted, slimmed with
   `simslim on <udid> --profile <profile>`, and — while still booted —
   the resulting `launchctl print-disabled system` set and running
   launchd-service count are captured into the image's metadata
   (`disabled_services`, `disabled_count`, `post_slim_procs`) before the
   builder is shut down again. When the installed simslim has the
   `verify` command (v0.6.0+), the daemon additionally runs
   `simslim verify <udid> --profile <profile>` — an exact profile-match
   check that lists any drifted daemons — and fails the build on drift;
   older binaries rely on the launchctl capture alone. Profile
   names are bare identifiers resolving to `~/qa/<name>.json` (the
   fleet's deployed location) — never paths, since the value comes off
   the wire; `"default"` uses simslim's built-in profile. On hosts without a simslim binary (searched at
   `$MANZANASD_SIMSLIM`, `~/bin/simslim`, `~/simtest/bin/simslim`, `$PATH`),
   builds with a `slim_profile` are refused with `unavailable` — build an
   unslimmed image instead.
3. The shutdown builder's data directory is archived (see format below)
   into the on-disk store (`<state-dir>/images/`, default
   `~/.manzanasd/images/`), indexed in `images.json`.
4. The builder sim is deleted — on success **and** on failure; the build
   flow only ever touches the sim it created.

Building refuses to archive a booted sim (`409 target_busy`): a booted
sim's data directory is being mutated by hundreds of daemons and would
archive torn state. The check runs before **and after** archiving — on a
shared host another agent can boot the hidden builder mid-pack, and a
post-pack re-verify discards the (possibly torn) archive in that case.

## Stamp

`POST /v0/images/{id}/stamp` `{"count","name_prefix?"}` (id or name) →
`201 {ImageStampResult}` with the created sims and the wall-clock
`duration_ms`.

For each of `count` (max 16) sims, the daemon creates a fresh simulator of
the image's device type/runtime named `<name_prefix>-<n>` (default
`manzanas-<n>`) and replaces its data directory with the archived one —
the exact `Runner.ReplaceDir` staging/rename plumbing the state engine's
snapshot restore uses, so a failed copy never leaves a torn data dir. The
archive is extracted once per stamp call and shared by all N sims.

**Slim state does not travel with the data directory.** On iOS 18+
runtimes the launchctl disables simslim applies are keyed to the sim's
UDID *outside* its data dir, so a freshly stamped sim would boot
effectively stock (every daemon back) even though its data came from a
slim image. For images with a `slim_profile`, the stamp flow therefore
boots each still-hidden sim, re-applies the image's recorded
`disabled_services` with `launchctl disable system/<svc>` (persistent
across reboots; daemons are never SIGKILLed — respawn churn costs more
than the daemon), verifies via `launchctl print-disabled system` — plus
an exact `simslim verify` drift check against the image's profile when
the installed simslim supports it (v0.6.0+) — and
shuts the sim down again. A sim whose disables cannot be verified fails
the whole stamp (full rollback) rather than leasing out an unslimmed sim
labelled slim. A slim image with no recorded `disabled_services` (built
before disables were captured) is refused outright (`409 unavailable` —
rebuild the image). Stamping still skips the ~25 s first-boot migration.

Every committed slim-stamped sim is recorded in
`<state-dir>/images/slimmed.json` (udid → disabled services): `simctl
erase` wipes the per-UDID launchctl config, so the state engine
re-applies the recorded set (boot → disable → verify → shutdown) after
every erase manzanasd performs — client `state.erase` calls and lease
auto-resets alike.

Each sim is created under a hidden `manzanasd-img-stamp-*` name and only
renamed to `<name_prefix>-<n>` after its data dir has been replaced, so
the registry never enumerates (and the lease manager never hands out) a
half-provisioned sim. Stamped sims keep their **own** UDID and
`device.plist` identity (the archived `device.plist` is provenance
metadata and is never extracted), so once renamed they appear in the
registry as ordinary leasable targets. On any failure every sim the call
created is deleted again — a stamp is all-or-nothing, and it never
touches sims it didn't create.

The booted-sim guardrail is enforced twice: once per sim right after
`simctl create`, and again for the whole batch just before the commit
renames. The per-sim check alone is check-then-act — on a shared host
another agent can `simctl boot` a hidden `manzanasd-img-stamp-*` sim
between the check and the data-dir swap — so if any sim is not
`Shutdown` at commit time, the stamp fails with `409 target_busy` and
rolls back completely (shutdown + delete of every sim it created).

## Delete

`DELETE /v0/images/{id}` removes the archive + index entry. Sims already
stamped from the image are unaffected: each owns a full copy of the data.

WS mirrors: `images.build`, `images.list`, `images.stamp` (params
`{"id", "count", "name_prefix"}`), `images.delete` (params `{"id"}`).

Images are fleet-level resources like targets — no lease guards these
routes, because build/stamp/delete only ever operate on simulators the
image flow itself creates.

## Integrity

Builds record the archive's SHA-256 in the index (`"sha256"`), and every
stamp re-hashes the archive and refuses on mismatch (`409 unavailable`,
not `500`: it is bad on-disk state, not a daemon fault — rebuild or
delete the image) — a swapped or
corrupted archive can never become leasable sim state. Copying an image
between hosts therefore means copying both the archive and its index
entry (or rebuilding the entry with the matching digest).

## Crash recovery

At store construction (daemon start) a best-effort sweep reclaims stale
files immediately and orphaned sims in the background (so a slow simctl
never delays the daemon's listen socket). It covers
leftovers from a build/stamp interrupted by a crash or kill: orphaned
staging dirs (`<id>.stamp-*`), half-written archives (`*.tar.zst.tmp`),
and shutdown simulators still carrying a hidden `manzanasd-img-*` name.
All three exist only while an operation is in flight, and the image flow
is the sole creator of that device-name shape, so the sweep never touches
anything it didn't make.

## Archive format

`<state-dir>/images/<img_id>.tar.zst` — a zstd-compressed tar:

| entry | contents |
|---|---|
| `data/...` | the builder sim's full data directory tree |
| `device.plist` | the builder's device.plist (provenance only; never extracted) |

Sockets/pipes are skipped (sims hold unix sockets that cannot and need not
be archived); file modes and in-tree symlinks are preserved, while
symlinks pointing outside the data dir (host-specific paths) are skipped
at pack time so every archive the daemon writes is also extractable.
Extraction validates entry names and symlink targets against traversal
and caps the total decompressed size at 64 GiB — archives are plain
files, copyable between hosts, so a tampered one must not be able to
write outside the staging dir or fill the disk. Metadata lives in
`images.json`:

```json
{"images": [{"id": "img_a1b2c3", "name": "clean", "device_type": "iPhone 17",
             "runtime": "iOS 26.5", "slim_profile": "agent-qa",
             "disabled_services": ["com.apple.apsd", "com.apple.diagnosticd", "…"],
             "disabled_count": 141, "post_slim_procs": 77,
             "size_bytes": 123456, "sha256": "9f86d081…",
             "created_at": "2026-08-01T00:00:00Z"}]}
```

The `runtime` field stores whatever the build request supplied (display
name or identifier); display names are re-resolved to identifiers at
every stamp. `disabled_count` and `post_slim_procs` are the measured
post-slim footprint (running launchd services on the booted builder) for
capacity planning; both are absent on unslimmed images.

## Profiles and in-app purchases

As of simslim v0.6.0, `--except store` (or `store` in a committed
profile's `except` array) reliably keeps the AMS payment-sheet daemons
enabled, so in-app purchase flows work on a slimmed sim — earlier
versions could break the payment sheet even with `store` excepted. The
reference fleet's `agent-qa` profile already lists `store` in its `except` array,
so purchase QA works on `agent-qa` sims once the host's simslim is
v0.6.0+; profiles that do NOT except `store` should get a dedicated
variant for purchase QA rather than changing a shared profile for
everyone (store daemons cost extra memory on every sim).

A drifted or suspect sim can also be checked manually with
`simslim verify <udid> --profile ~/qa/<name>.json` — it exits non-zero
and lists drift in both directions; re-run `simslim on` to repair.

## Tradeoffs vs. snapshots

Snapshots (docs/state.md) and golden images share the data-dir-swap
restore mechanism but solve different problems:

| | snapshot (`simctl clone`) | golden image (tar.zst archive) |
|---|---|---|
| scope | restore **one existing** sim to a prior state | stamp out **N new** sims |
| storage | live clone device (APFS block-shared, near-free) | self-contained compressed archive (survives `simctl delete unavailable`, copyable between hosts of the same runtime) |
| speed | restore ≈ seconds (clonefile) | stamp ≈ seconds per sim (`create` + extract-once + copy) |
| coupling | clone must stay registered with CoreSimulator | none — plain file; but the target host needs the same runtime installed |
| identity | tied to the source UDID | portable to any fresh sim of the same device type/runtime |

A tar.zst archive is chosen over keeping a template clone device because it
is (a) immune to Xcode/CoreSimulator cleanup deleting the backing device,
(b) 3–10× smaller than the raw data dir, and (c) a plain file that can be
shipped to other fleet hosts. The cost is one decompress per stamp call
(amortized across the N sims of that call).
