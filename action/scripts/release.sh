#!/bin/bash
# Release the lease (idempotent on the daemon side).
set -euo pipefail

if [[ -n "${MANZANAS_RENEW_PID:-}" ]]; then
  kill "$MANZANAS_RENEW_PID" 2>/dev/null || true
fi

ADDR="${MANZANASD_ADDR%/}"
curl -fsS -X DELETE "$ADDR/v0/leases/$MANZANAS_LEASE" >/dev/null \
  && echo "released lease $MANZANAS_LEASE" \
  || echo "release failed for $MANZANAS_LEASE (may already be expired)" >&2
