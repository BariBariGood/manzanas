#!/bin/bash
# Acquire a lease from the daemon, waiting in the queue if necessary.
# Emits lease_id and target_udid as step outputs.
set -euo pipefail

ADDR="${MANZANASD_ADDR%/}"
# ${arr[@]+...} guard: bash 3.2 (macOS /bin/bash) treats an empty array as
# unbound under `set -u`.
IFS=',' read -r -a LABELS_ARR <<< "${LEASE_LABELS:-}"
LABELS_JSON="$(printf '%s\n' ${LABELS_ARR[@]+"${LABELS_ARR[@]}"} | sed '/^$/d' | jq -R . | jq -sc .)"

BODY="$(jq -nc \
  --argjson labels "$LABELS_JSON" \
  --arg agent "${AGENT_ID:-github-actions}" \
  --argjson ttl "${LEASE_TTL:-600}" \
  '{labels: $labels, agent_id: $agent, ttl_seconds: $ttl, purpose: "github-action"}')"

RESP="$(curl -fsS -X POST "$ADDR/v0/leases" -d "$BODY")"
LEASE_ID="$(jq -r .id <<< "$RESP")"
STATE="$(jq -r .state <<< "$RESP")"
echo "lease $LEASE_ID state=$STATE"

# Record the lease id immediately so the always() release step can clean up,
# and delete the lease ourselves if this script dies before going active.
echo "lease_id=$LEASE_ID" >> "$GITHUB_OUTPUT"
trap 'curl -fsS -X DELETE "$ADDR/v0/leases/$LEASE_ID" >/dev/null 2>&1 || true' ERR

# Poll until active (queued leases must poll to stay alive).
DEADLINE=$(( $(date +%s) + 1800 ))
while [[ "$STATE" == "queued" ]]; do
  if (( $(date +%s) > DEADLINE )); then
    echo "timed out waiting for lease to become active" >&2
    curl -fsS -X DELETE "$ADDR/v0/leases/$LEASE_ID" >/dev/null 2>&1 || true
    exit 1
  fi
  sleep 5
  RESP="$(curl -fsS "$ADDR/v0/leases/$LEASE_ID")"
  STATE="$(jq -r .state <<< "$RESP")"
done

if [[ "$STATE" != "active" ]]; then
  echo "lease entered unexpected state: $STATE" >&2
  curl -fsS -X DELETE "$ADDR/v0/leases/$LEASE_ID" >/dev/null 2>&1 || true
  exit 1
fi
trap - ERR

TARGET_UDID="$(jq -r .target_udid <<< "$RESP")"
echo "leased target $TARGET_UDID"

# Background keep-alive: renew at ~TTL/3 so scripts longer than the TTL
# don't lose the target mid-run. release.sh kills it. Use the TTL the
# daemon actually granted (it clamps to its own max), not the request.
TTL="$(jq -r '.ttl_seconds // empty' <<< "$RESP")"
[[ -n "$TTL" ]] || TTL="${LEASE_TTL:-600}"
INTERVAL=$(( TTL / 3 )); (( INTERVAL < 10 )) && INTERVAL=10
# Absolute cap so an orphaned renewer (cancelled job, killed runner) cannot
# hold the target forever; release.sh normally kills it much sooner.
MAX_RENEW_SECONDS=${MANZANAS_MAX_RENEW_SECONDS:-7200}
(
  FAILS=0
  RENEW_DEADLINE=$(( $(date +%s) + MAX_RENEW_SECONDS ))
  while sleep "$INTERVAL"; do
    if (( $(date +%s) > RENEW_DEADLINE )); then exit 0; fi
    if curl -fsS -X POST "$ADDR/v0/leases/$LEASE_ID/renew" -d '{}' >/dev/null 2>&1; then
      FAILS=0
    else
      FAILS=$((FAILS + 1))
      if (( FAILS >= 3 )); then exit 0; fi
    fi
  done
) </dev/null >/dev/null 2>&1 &
RENEW_PID=$!
disown "$RENEW_PID" 2>/dev/null || true

{
  echo "target_udid=$TARGET_UDID"
  echo "renew_pid=$RENEW_PID"
} >> "$GITHUB_OUTPUT"
