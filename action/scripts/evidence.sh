#!/bin/bash
# Export the run's journal as PR-ready markdown evidence and add it to the
# job step summary. Tolerates journal-disabled daemons (501) and daemons
# that predate the journal slice (404).
set -euo pipefail

OUT="$RUNNER_TEMP/manzanasd-evidence.md"

if manzanas journal export "$MANZANAS_LEASE" -o "$OUT"; then
  echo "path=$OUT" >> "$GITHUB_OUTPUT"
  cat "$OUT" >> "$GITHUB_STEP_SUMMARY"
  echo "journal evidence saved to $OUT"
else
  echo "journal evidence unavailable; daemon may run with the journal disabled" >&2
  echo "path=" >> "$GITHUB_OUTPUT"
fi
