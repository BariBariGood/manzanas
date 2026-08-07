# manzanasd GitHub Action

Composite action that leases an iOS simulator from a `manzanasd` daemon,
runs a script with the lease in the environment, captures a screenshot
artifact, releases the lease, and (optionally) posts a PR comment linking
the screenshot artifact.

```yaml
jobs:
  sim-test:
    runs-on: [self-hosted, macos]   # or any runner that can reach the daemon
    permissions:
      pull-requests: write          # only if comment-pr: true
    steps:
      - uses: BariBariGood/manzanas/action@v0.2.0
        with:
          daemon-addr: http://100.64.0.1:7433
          labels: ios26,iphone-17-pro
          manzanas-version: v0.2.0    # pin an exact release tag
          comment-pr: "true"
          journal-evidence: "true"
          run: |
            echo "holding lease $MANZANAS_LEASE on $MANZANAS_TARGET_UDID"
            curl -fsS -X POST "$MANZANASD_ADDR/v0/targets/$MANZANAS_TARGET_UDID/boot" \
              -d "{\"lease_id\":\"$MANZANAS_LEASE\"}"
            # ... build, install, drive the app ...
```

## Environment provided to `run`

- `MANZANASD_ADDR` — the daemon address
- `MANZANAS_LEASE` — the active lease id
- `MANZANAS_TARGET_UDID` — the UDID of the leased target

## Security

`run` executes as a shell script on the runner and the other inputs feed
curl/jq invocations. Never build these inputs from untrusted data
(PR titles/bodies, `github.event.*` on `pull_request_target`, etc.) —
that hands arbitrary command execution and the job's token to whoever
controls the data. Use literal values or trusted repo variables only.

## Pinning strategy

- Pin the **action** by tag (`@v0.2.0`) or commit SHA.
- Pin `manzanas-version` to the same release tag so the CLI matches the
  daemon's protocol version. `latest` resolves the newest GitHub Release at
  run time and is only for experimentation.

## Screenshot + PR comment

The screenshot is captured via `POST /v0/actions` with `kind: screenshot`
and uploaded as the `manzanasd-screenshot-<job>` artifact (override with
the `artifact-name` input, e.g. for matrix jobs). Daemons that predate
the actions slice return `501 not_implemented`; the step degrades
gracefully. `comment-pr: true` posts a comment linking the run's artifact
(needs `pull-requests: write`).

## Journal evidence

`journal-evidence: "true"` runs `manzanas journal export` on the lease's
run after the script: the PR-ready markdown summary (step table with
failures highlighted, artifact digests) is appended to the job step
summary and exposed as the `evidence-path` output. With `comment-pr:
"true"` it is also posted as a PR comment. Daemons running with the
journal disabled degrade gracefully (the step logs a note and sets an
empty path). See `docs/journal.md`, "Exporting PR evidence" — including
the security posture: self-hosted runners must never run untrusted fork
PR code, and daemons should stay tailnet-only or localhost.
