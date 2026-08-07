## manzanasd run journal — `lse_smoke1`

| | |
|---|---|
| Format | `journal/v0` |
| Agent | agent-journal |
| Purpose | slice-5 smoke test |
| Target | iPhone 17 Pro (`AAAA-1111`) |
| Runtime | iOS 26.5 |
| Device type | iPhone 17 Pro |
| Started | 2026-08-01 11:59:00 UTC |
| Entries | 6 |
| Result | **FAILED** (1 of 6 steps errored) |

### Actions

| # | Time (UTC) | Kind | Action | Status | Detail |
|---|---|---|---|---|---|
| 1 | 2026-08-01 12:00:00 | lease | leases.acquire | ok | `labels=[ios26]` `ttl_seconds=300` |
| 2 | 2026-08-01 12:00:00 | action | targets.boot | ok | `udid=AAAA-1111` |
| 3 | 2026-08-01 12:00:00 | observation | journal.artifact | ok | `name=boot.png` |
| 4 | 2026-08-01 12:00:00 | segment | recording.stop | ok | `bytes=2.097152e+06` `codec=hevc` `duration_s=12.5` `reason=stopped` `udid=AAAA-1111` |
| 5 | 2026-08-01 12:00:00 | action | targets.shutdown | **error** | sim wedged |
| 6 | 2026-08-01 12:00:00 | lease | leases.release | ok |  |

### Artifacts

- `artifacts/deadbeef00112233.png` (step 3, sha256 `deadbeef0011…`)
- `artifacts/cafef00d8899aabb.mp4` (step 4, sha256 `cafef00d8899…`, 12.5s 2.0 MiB video, stopped)
