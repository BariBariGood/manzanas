#!/bin/bash
# Post the exported journal evidence as a PR comment (requires
# pull-requests: write). Long summaries are truncated to GitHub's comment
# body cap, and a failed post never fails the job — the step summary
# already carries the full document.
set -euo pipefail

if [[ -z "${EVIDENCE_PATH:-}" || ! -f "$EVIDENCE_PATH" ]]; then
  echo "no evidence to post"
  exit 0
fi

# GitHub caps comment bodies at 65536 characters; leave room for the note.
BODY_FILE="$RUNNER_TEMP/evidence-comment.md"
if [[ "$(wc -c < "$EVIDENCE_PATH")" -gt 65000 ]]; then
  head -c 65000 "$EVIDENCE_PATH" > "$BODY_FILE"
  printf '\n\n_…truncated; the full evidence is in the [job step summary](%s/%s/actions/runs/%s)._\n' \
    "$GITHUB_SERVER_URL" "$GITHUB_REPOSITORY" "$GITHUB_RUN_ID" >> "$BODY_FILE"
else
  cp "$EVIDENCE_PATH" "$BODY_FILE"
fi

if gh api "repos/${GITHUB_REPOSITORY}/issues/${PR_NUMBER}/comments" \
  -F body=@"$BODY_FILE" >/dev/null; then
  echo "posted evidence comment on #$PR_NUMBER"
else
  echo "failed to post evidence comment; full evidence is in the job step summary" >&2
fi
