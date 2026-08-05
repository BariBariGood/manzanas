#!/bin/bash
# Post the captured screenshot as a PR comment (requires pull-requests: write).
set -euo pipefail

if [[ -z "${SCREENSHOT_PATH:-}" || ! -f "$SCREENSHOT_PATH" ]]; then
  echo "no screenshot to post"
  exit 0
fi

# GitHub's comment API cannot host images directly; embed as a data-free
# note + attach via artifact link. Post a comment referencing the run.
BODY="manzanasd simulator run on target \`${TARGET_UDID:-unknown}\` — screenshot uploaded as the \`${ARTIFACT_NAME:-manzanasd-screenshot}\` artifact on run [${GITHUB_RUN_ID}](${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID})."

gh api "repos/${GITHUB_REPOSITORY}/issues/${PR_NUMBER}/comments" \
  -f body="$BODY" >/dev/null
echo "posted PR comment on #$PR_NUMBER"
