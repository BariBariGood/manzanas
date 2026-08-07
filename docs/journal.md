# Run journal — format v0

The run journal is manzanasd's exportable evidence layer: every protocol
action performed under a lease is recorded as an append-only JSONL entry,
with artifacts (screenshots, video segment refs) stored content-addressed
alongside. Format v0 is designed so a run can be **replayed** against an
equivalent target in v0.2 — each run carries determinism metadata (target
UDID, runtime, device type, app versions when known).

## Versioning rules

- The format identifier is `journal/v0`, stored in each run's `meta.json`
  as `format_version`.
- Within v0, changes must be **additive**: new payload keys, new entry
  kinds, and new meta fields may appear; existing keys never change meaning
  or type. Readers must ignore unknown keys.
- Breaking changes (renaming/removing keys, changing seq semantics) bump to
  `journal/v1`. Readers refuse formats they don't know: reading a run whose
  `meta.json` declares an unrecognized `format_version` fails
  (`501 not_implemented` over the API) instead of misinterpreting it as v0.
- v0.2 replay requirement: entries record enough to re-drive a run — the
  ordered action list with params, plus meta pinning the target
  (UDID/runtime/device type) and app versions. A replayer matches an
  equivalent target by runtime + device type and re-dispatches each
  `action`-kind entry in seq order.

## On-disk layout

One directory per run under the journal root (default
`~/.manzanasd/journal`, flag `--journal-dir` / env `MANZANASD_JOURNAL_DIR`).
The run ID **is** the lease ID.

```
~/.manzanasd/journal/
  lse_ab12cd34/                 # one run == one lease
    meta.json                   # RunMeta: determinism metadata
    entries.jsonl               # append-only, one Entry per line
    artifacts/
      <sha256>.png              # content-addressed artifacts
      <sha256>.mp4
```

### meta.json (RunMeta)

Written when the lease becomes active:

```json
{
  "format_version": "journal/v0",
  "run_id": "lse_ab12cd34",
  "lease_id": "lse_ab12cd34",
  "agent_id": "claude-1",
  "purpose": "onboarding smoke",
  "target_udid": "AAAA-1111-...",
  "target_name": "iPhone 17 Pro",
  "runtime": "iOS 26.5",
  "device_type": "iPhone 17 Pro",
  "app_versions": {"com.example.app": "1.4.2"},
  "daemon_version": "v0",
  "created_at": "2026-08-01T11:59:00Z"
}
```

`app_versions` is filled when the actions/state slices report installed app
versions; it may be empty.

### entries.jsonl (Entry)

One JSON object per line; `ref.seq` is 1-based and strictly increasing per
run (recovered by scanning on daemon restart). Torn/corrupt lines are
skipped by readers; oversized lines are skipped too, but their seq is still
recovered from the line's prefix so a restart never reuses it.

```json
{"ref":{"run_id":"lse_ab12cd34","seq":2},"kind":"action","payload":{
  "ts":"2026-08-01T12:00:03.412Z",
  "lease_id":"lse_ab12cd34",
  "agent_id":"claude-1",
  "action":"targets.boot",
  "params":{"udid":"AAAA-1111-..."},
  "status":"ok",
  "ax_before":"sha256-of-a11y-tree-before",
  "ax_after":"sha256-of-a11y-tree-after",
  "artifacts":[{"path":"artifacts/deadbeef....png","sha256":"deadbeef...","bytes":48213}]
}}
```

Entry **kinds**: `lease` (lease lifecycle), `action` (HID/target ops),
`observation` (a11y describes, screenshots), `state` (snapshot/restore/
erase/fixture/snapshot-delete ops and the post-lease auto-reset,
`state.reset`), `segment` (video segment refs). Payload keys:

| Key | When | Meaning |
|---|---|---|
| `ts` | always | RFC 3339 UTC, stamped at append |
| `lease_id` | always | the run's lease |
| `action` | always | protocol method, e.g. `leases.acquire`, `leases.expire`, `targets.boot`, `state.snapshot` |
| `status` | always | `ok` or `error` |
| `agent_id` | when known | lease holder |
| `params` | when present | request params (opaque, backend-owned schema) |
| `error` | on error | error message |
| `ax_before` / `ax_after` | when the actions backend provides them | a11y tree hashes around the action (HID actions: opt-in via payload `ax_hashes: true`; an `observe`'s tree hash is recorded as `ax_after`) |
| `artifacts` | when present | list of `{path, sha256, bytes}` refs into `artifacts/` |

Artifacts are content-addressed (`<sha256><ext>`) and referenced by
run-relative path, so entries stay valid across copies of the run dir and
identical content dedupes.

## Recording model

The server wraps protocol handlers with a thin `journal.Recorder`
middleware (`internal/journal/recorder.go`): recording happens generically
at the protocol layer, not per-backend, so the actions/state/stream slices
get journaling for free. Backends can enrich entries (a11y hashes,
artifacts) by returning them with results, and can ingest artifacts via
`POST /v0/journal/{run}/artifacts`. A successful `screenshot` action's PNG
is stored as a run artifact and referenced from the entry, so
`inline:false` callers can still fetch the capture (journal-enabled
daemons only). Recording is best-effort and never fails the recorded
operation.

## Retention / GC

Runs are the GC unit (entries + artifacts stay consistent). Bounds, both
optional:

- `--journal-max-age` (env `MANZANASD_JOURNAL_MAX_AGE`, default 168h): runs
  whose newest file is older are removed.
- `--journal-max-bytes` (env `MANZANASD_JOURNAL_MAX_BYTES`, default 2 GiB):
  oldest runs are removed until the journal fits.

Runs whose lease is still live are never reclaimed, regardless of either
bound: the server marks a run open at lease grant and closed when the
lease ends. Runs with live `journal.tail` subscribers are also skipped.
If a run's files are reclaimed while the daemon still holds its seq
counter, the counter is preserved, so seqs stay strictly increasing and
journal refs already handed to clients are never reissued.

## Retrieval API

See `proto/PROTOCOL.md` §7:

- `GET /v0/journal` — list runs.
- `GET /v0/journal/{run}?from_seq=&limit=` — paginated entries + meta.
- `GET /v0/journal/{run}/artifacts/{path}` — fetch an artifact.
- `POST /v0/journal/{run}/artifacts?name=&kind=` — ingest an artifact
  (`manzanas journal upload RUN_ID FILE...`).
- `GET /v0/journal/{run}/export.md` — PR-comment-ready markdown evidence
  (run metadata, action table, artifact list).
- WS `journal.tail` — replay from `from_seq`, then live entries as
  `journal.entry` events (for `manzanas journal tail`).

## Exporting PR evidence

`manzanas journal export` turns a run into a self-contained, PR-ready
evidence document: run metadata, the seq-ordered step table with durations
implied by per-step timestamps and failures highlighted, and the artifact
list (screenshots, video segments) with content digests.

```sh
manzanas journal export $RUN_ID                    # markdown to stdout
manzanas journal export $RUN_ID -o evidence.md     # write to a file
manzanas journal export $RUN_ID --format json      # meta + entries + failure count
manzanas journal export $RUN_ID --journal-dir ~/.manzanasd/journal   # offline, no daemon
```

The run ID is the lease ID. The default path pages
`GET /v0/journal/{run}` on the daemon; `--journal-dir` reads the on-disk
store directly, so evidence can be exported after the daemon has stopped
(or from a copied run directory). `GET /v0/journal/{run}/export.md`
returns the same markdown over HTTP, and MCP agents get it via the
`journal_export` tool.

Sample output:

```markdown
## manzanasd run journal — `lse_ab12cd34`

| | |
|---|---|
| Format | `journal/v0` |
| Agent | claude-1 |
| Purpose | onboarding smoke |
| Target | iPhone 17 Pro (`AAAA-1111`) |
| Runtime | iOS 26.5 |
| Device type | iPhone 17 Pro |
| Started | 2026-08-01 11:59:00 UTC |
| Entries | 4 |
| Result | **FAILED** (1 of 4 steps errored) |

### Actions

| # | Time (UTC) | Kind | Action | Status | Detail |
|---|---|---|---|---|---|
| 1 | 2026-08-01 12:00:00 | lease | leases.acquire | ok | `labels=[ios26]` `ttl_seconds=300` |
| 2 | 2026-08-01 12:00:01 | action | targets.boot | ok | `udid=AAAA-1111` |
| 3 | 2026-08-01 12:00:09 | observation | screenshot | ok | `format=png` |
| 4 | 2026-08-01 12:00:12 | action | tap | **error** | element not found |

### Artifacts

- `artifacts/deadbeef00112233.png` (step 3, sha256 `deadbeef0011…`)
```

Artifact bullets carry the run-relative path and digest; fetch the bytes
with `GET /v0/journal/{run}/artifacts/{path}` while the run is retained.
Cite evidence as "run `<run_id>`, artifact `<path>`" (see
[agent-qa.md](agent-qa.md), "QA evidence durability").

### CI: attach evidence to a PR

The composite action (`action/`) has this built in — set
`journal-evidence: "true"` and the exported markdown is added to the job
step summary, plus posted as a PR comment when `comment-pr: "true"`.
Without the action, the recipe is: lease → run → export → comment:

```yaml
jobs:
  sim-qa:
    runs-on: [self-hosted, macos, tailnet]   # must reach the daemon
    permissions:
      pull-requests: write
    steps:
      - uses: BariBariGood/manzanas/action@v0.2.0
        with:
          daemon-addr: http://100.64.0.1:7433
          labels: ios26
          journal-evidence: "true"
          comment-pr: "true"
          run: |
            # ... drive the app under $MANZANAS_LEASE ...
            manzanas journal upload "$MANZANAS_LEASE" my-shot.png
```

or as raw steps with `GITHUB_TOKEN`:

```sh
manzanas journal export "$MANZANAS_LEASE" -o evidence.md
gh api "repos/$GITHUB_REPOSITORY/issues/$PR_NUMBER/comments" -F body=@evidence.md
```

**Security posture, stated plainly:** the daemon trusts its network (see
below), so any CI runner that can reach it holds full lease/action power
over the fleet. Self-hosted runners must **never** execute untrusted fork
PR code — use them only on private repos (or with fork PR workflows
disabled), and keep daemons tailnet-only or localhost so a compromised
cloud runner can't reach them. Never feed untrusted data (PR titles,
`github.event.*` on `pull_request_target`) into the action's inputs.

## Authorization (v0 limitation)

The v0 protocol carries no caller identity — the lease ID is the only
credential — so journal reads are unauthenticated and artifact writes are
gated only on the run's lease still being active. Deployments rely on
network-level trust (tailnet/localhost). Agent-scoped read/write
authorization is deferred to a protocol-wide authentication/identity
slice.
