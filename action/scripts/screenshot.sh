#!/bin/bash
# Capture a screenshot of the leased target via the actions API.
# Tolerates daemons that have not implemented the actions slice yet (501).
set -euo pipefail

ADDR="${MANZANASD_ADDR%/}"
OUT="$RUNNER_TEMP/manzanasd-screenshot.png"

BODY="$(jq -nc \
  --arg lease "$MANZANAS_LEASE" \
  '{lease_id: $lease, kind: "screenshot", payload: {}}')"

HTTP_CODE="$(curl -sS -o "$RUNNER_TEMP/shot-resp.json" -w '%{http_code}' \
  -X POST "$ADDR/v0/actions" -d "$BODY" || echo 000)"

if [[ "$HTTP_CODE" == "200" ]]; then
  # The screenshot action returns the base64 PNG as .result.png_base64.
  if jq -re '.result.png_base64' "$RUNNER_TEMP/shot-resp.json" >/dev/null 2>&1; then
    jq -r '.result.png_base64' "$RUNNER_TEMP/shot-resp.json" | base64 -d > "$OUT"
    echo "path=$OUT" >> "$GITHUB_OUTPUT"
    echo "screenshot saved to $OUT"
    exit 0
  fi
fi

echo "screenshot unavailable (HTTP $HTTP_CODE); daemon may predate the actions slice" >&2
echo "path=" >> "$GITHUB_OUTPUT"
