---
name: journal-evidence-export
description: Fetch run evidence from the manzanasd journal - list runs, page entries, download screenshot artifacts, upload your own evidence, and export a PR-ready markdown summary. Use when a task needs proof of what was done on a simulator (QA evidence for a PR, debugging a run).
---

# Journal evidence export

Every mutating op under a lease is journaled (append-only JSONL + content-
addressed artifacts). The run ID **is** the lease ID, so you always know your
run without extra bookkeeping. Journal-disabled daemons (`--journal-dir ""`)
answer `501` on all of these.

## The one-call PR evidence export

```sh
curl -s $D/v0/journal/$LID/export.md > evidence.md
```

Returns a PR-comment-ready markdown summary: run metadata (agent, purpose,
target, runtime), an ordered action table with status, and the artifact list.
Paste it into the PR body or a comment.

## Inspect a run

```sh
curl -s $D/v0/journal | jq '.runs[0]'                  # newest runs first
curl -s "$D/v0/journal/$LID?limit=100" | jq '.entries[] | .payload.action'
# next_seq != 0 → pass it back as from_seq for the next page
```

Entry kinds: `lease`, `action`, `observation`, `state`, `segment`. HID
actions record `ax_before`/`ax_after` tree hashes when dispatched with
payload `"ax_hashes": true` — cheap before/after UI evidence.

## Fetch artifacts (screenshots)

A successful `screenshot` action is stored automatically; entries carry
`artifacts: [{path, sha256, bytes}]`:

```sh
curl -s "$D/v0/journal/$LID?limit=1000" \
  | jq -r '.entries[].payload.artifacts[]?.path'
curl -s -o shot.png "$D/v0/journal/$LID/artifacts/artifacts/<sha256>.png"
```

Tip: dispatch screenshots with `"inline": false` to keep responses small and
pull the bytes from the journal afterwards.

## Upload your own evidence

While the lease is still active (immutable after release/expiry — upload
before you release):

```sh
curl -s -X POST "$D/v0/journal/$LID/artifacts?name=final-state&kind=screenshot" \
  --data-binary @shot.png
```

`kind`: `observation` (default), `screenshot`, or `video`.

## Live tail

`manzanas --daemon $D journal tail $LID --follow` (WS `journal.tail` replays
from `from_seq` then streams new entries).

## Retention

Runs are GC'd by `--journal-max-age` (default 7 days) and
`--journal-max-bytes` (default 2 GiB); live-lease runs are never reclaimed.
Export evidence promptly — don't rely on the journal as long-term storage.
