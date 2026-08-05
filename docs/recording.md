# Video recording

manzanasd can record a leased simulator's screen to an `.mp4` that lands in
the run journal as a content-addressed artifact. Recording is daemon-managed:
the daemon owns the `simctl io <udid> recordVideo` child, finalizes it safely,
validates the output, and ingests it — agents never touch simctl directly.

## Quick start

```sh
# manual start/stop
LEASE=$(manzanas --json lease acquire --labels ios26 | jq -r .id)
manzanas boot <udid> --lease $LEASE
manzanas record start --lease $LEASE [--codec hevc|h264] [--max-seconds N]
# ... drive the app ...
manzanas record stop --lease $LEASE
# -> artifact path + download URL; also in `manzanas journal tail $LEASE`

# or record the whole lease automatically
manzanas lease acquire --labels ios26 --record video
```

MCP agents get the same pair as `record_start` / `record_stop`, plus a
`record: "video"` option on `lease_acquire`.

## HTTP API

- `POST /v0/targets/{udid}/recording` — body `{lease_id, codec?, max_seconds?}`.
  Requires an active lease on the target and target state `Booted`
  (`recordVideo` against a shut-down sim exits 0 with a 0-byte file, so
  non-Booted starts are refused with `409 target_busy`). One recording per
  target; duplicates get `409`. Returns `201 {recording_id, codec, max_seconds, ...}`.
- `POST /v0/targets/{udid}/recording/stop` — body `{lease_id}`. Returns the
  artifact ref, journal ref, codec, duration, bytes, and stop reason.

The finalized video is appended to the run journal as a `kind:"segment"` /
`action:"recording.stop"` entry and stored under `artifacts/<sha256>.mp4`;
download it via `GET /v0/journal/{run}/artifacts/{path}`. `export.md`
annotates recording artifacts with duration, size, and stop reason.

## Lifecycle and auto-stop

The recorder child is stopped with SIGINT (the only clean finalize signal),
reaped for up to 10 s, then SIGKILLed as a last resort. A SIGKILL leaves the
simulator's recording service poisoned — further recordings on that target
are refused with `409` until it is shut down and booted again (the normal
lease reset / pool recycle path does this).

A recording never outlives its lease or its simulator. It is stopped and
ingested automatically with the corresponding `reason`:

| reason            | trigger                                             |
|-------------------|-----------------------------------------------------|
| `stopped`         | explicit stop call                                  |
| `lease_end`       | lease release or expiry                             |
| `target_shutdown` | `manzanas shutdown` on the target                    |
| `max_seconds`     | duration cap hit                                    |
| `max_bytes`       | size cap hit                                        |
| `daemon_shutdown` | daemon exiting                                      |
| `daemon_restart`  | orphan recovered after a daemon restart             |
| `exited`          | the recorder child exited on its own                |

Stops triggered by lease end run off the request path: releasing a lease
never blocks on mp4 finalization.

Output is validated before ingest (non-empty + `moov` atom present); invalid
spools are deleted and journaled as an error segment instead.

## Restart recovery

Each live recording persists `<journal>/<run>/tmp/recording.json` (pid,
spool path, start time). On startup the daemon SIGINTs orphaned recorder
children it spawned in a previous life, validates their spools, and ingests
salvageable ones with `reason:"daemon_restart"`. Stray `recordVideo`
processes the daemon did not spawn are logged and never signaled.

## Flags and limits

| flag | env | default | |
|---|---|---|---|
| `--record-max-seconds` | `MANZANASD_RECORD_MAX_SECONDS` | 300 | hard duration cap per recording |
| `--record-max-bytes` | `MANZANASD_RECORD_MAX_BYTES` | 128 MiB | hard size cap per recording |
| `--record-max-concurrent` | `MANZANASD_RECORD_MAX_CONCURRENT` | 2 | simultaneous recordings per daemon |

Starts are also refused when the journal volume has less than 10 GiB free.
The defaults are deliberately conservative for shared hosts where the fleet
daemon coexists with interactive use: recording adds an encoder process per
target, and HEVC (the default codec) keeps files several times smaller than
h264 at the same duration. Requested `max_seconds` above the daemon cap are
clamped down, never rejected.

Recording requires the journal (`--journal-dir`); with the journal disabled
the endpoints return `501 not_implemented`.
