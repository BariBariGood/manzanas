#!/usr/bin/env bash
# bench.sh — reproduce the README's warm-pool / warm-action numbers on your Mac.
#
# Runs the full benchmark against a scratch manzanasd on a spare port:
#
#   phase A (daemon with a 1-sim warm pool + warm actions):
#     thaw      lease grant on a parked sim (SIGCONT thaw included), N samples
#     tap/observe (warm)  actions through the resident simbridge helper
#     idle CPU  %CPU of the sim's process tree, parked vs booted-idle
#   phase B (same daemon, --no-warm, no pool):
#     grant     lease grant on a booted un-parked sim (baseline bookkeeping)
#     tap/observe (cold)  actions via a per-call AXe spawn
#     boot      shutdown -> boot cycles
#
# Usage:
#   eval/bench/bench.sh [--udid UDID] [--port 7434] [--samples 20] [--boot-samples 5]
#
# With no --udid it creates a throwaway iPhone sim on the newest iOS runtime
# and deletes it afterwards. Requires: macOS + Xcode (simctl), AXe for
# actions (https://github.com/cameroncooke/AXe), optionally simbridge
# (helpers/simbridge) for the warm path and simslim for a slimmed pool sim.
#
# Results land in eval/bench/out/ (bench.jsonl + bench-report.md).
set -euo pipefail

PORT=7434
SAMPLES=20
BOOT_SAMPLES=5
UDID=""
CREATED_SIM=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --udid) UDID="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --samples) SAMPLES="$2"; shift 2 ;;
    --boot-samples) BOOT_SAMPLES="$2"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

if [[ "$(uname)" != "Darwin" ]]; then
  echo "bench.sh drives real simulators; run it on a Mac with Xcode." >&2
  exit 1
fi

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="$ROOT/eval/bench/out"
mkdir -p "$OUT"
: > "$OUT/bench.jsonl"

GO="${GO:-go}"
echo "== building bin/manzanasd + bin/manzanas-bench"
(cd "$ROOT" && "$GO" build -o bin/manzanasd ./cmd/manzanasd && "$GO" build -o bin/manzanas-bench ./eval/cmd/manzanas-bench)

if [[ -z "$UDID" ]]; then
  DEVTYPE=$(xcrun simctl list devicetypes | grep -Eo 'com.apple.CoreSimulator.SimDeviceType.iPhone-[0-9]+' | sort -t- -k2 -n | tail -1)
  RUNTIME=$(xcrun simctl list runtimes | grep -Eo 'com.apple.CoreSimulator.SimRuntime.iOS-[0-9-]+' | tail -1)
  UDID=$(xcrun simctl create manzanas-bench "$DEVTYPE" "$RUNTIME")
  CREATED_SIM=1
  echo "== created scratch sim $UDID ($DEVTYPE, $RUNTIME)"
fi

DAEMON="http://localhost:$PORT"
STATE_DIR=$(mktemp -d)
JOURNAL_DIR=$(mktemp -d)
DPID=""

DLOG="$OUT/daemon.log"
start_daemon() { # args: extra daemon flags
  "$ROOT/bin/manzanasd" --addr ":$PORT" --state-dir "$STATE_DIR" \
    --journal-dir "$JOURNAL_DIR" "$@" >"$DLOG" 2>&1 &
  DPID=$!
  for _ in $(seq 1 300); do
    curl -fsS "$DAEMON/v0/healthz" >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "daemon did not become healthy; see $DLOG" >&2
  exit 1
}

stop_daemon() {
  [[ -n "$DPID" ]] && kill "$DPID" 2>/dev/null && wait "$DPID" 2>/dev/null || true
  DPID=""
}

cleanup() {
  stop_daemon
  if [[ "$CREATED_SIM" == 1 ]]; then
    xcrun simctl shutdown "$UDID" >/dev/null 2>&1 || true
    xcrun simctl delete "$UDID" >/dev/null 2>&1 || true
  fi
  rm -rf "$STATE_DIR" "$JOURNAL_DIR"
}
trap cleanup EXIT

# sim_cpu: sum %CPU over the sim's process tree, averaged over N samples.
sim_cpu() { # args: label samples interval
  local label=$1 n=$2 interval=$3 total=0 v
  for _ in $(seq 1 "$n"); do
    v=$( { ps -Ao pcpu=,command= | grep -F "$UDID" | grep -v grep || true; } | awk '{s+=$1} END {printf "%.1f", s+0}')
    total=$(echo "$total + $v" | bc)
    sleep "$interval"
  done
  local avg
  avg=$(echo "scale=1; $total / $n" | bc | awk '{printf "%.1f", $1}')
  echo "idle CPU ($label): ${avg}% (avg of $n samples, ${interval}s apart)"
  echo "{\"phase\":\"cpu_$label\",\"avg_pct\":$avg,\"n\":$n}" >> "$OUT/bench.jsonl"
}

BENCH="$ROOT/bin/manzanas-bench --daemon $DAEMON --udid $UDID --json $OUT/bench.jsonl"

echo "== phase A: warm pool + warm actions (daemon --pool-sims $UDID)"
DLOG="$OUT/daemon-a.log"
# Wait until the pool has parked the sim before benching: the daemon boots
# (and optionally slims) the pool sim at startup, which can take a while.
start_daemon --pool-sims "$UDID" ${SLIM_PROFILE:+--pool-slim-profile "$SLIM_PROFILE"} ${LOAD_FACTOR:+--pool-load-factor "$LOAD_FACTOR"}
echo "-- waiting for the pool sim to be booted + parked (first boot can take minutes)"
for _ in $(seq 1 600); do
  if curl -fsS "$DAEMON/v0/targets" 2>/dev/null | UDID="$UDID" /usr/bin/python3 -c '
import json, os, sys
ts = json.load(sys.stdin)["targets"]
ok = any(t["udid"] == os.environ["UDID"] and t.get("warm") for t in ts)
sys.exit(0 if ok else 1)
'; then break; fi
  sleep 2
done

$BENCH --phase thaw --samples "$SAMPLES"
sim_cpu parked 10 2
$BENCH --phase tap --label tap_warm --samples "$SAMPLES"
$BENCH --phase observe --label observe_warm --samples "$SAMPLES"
# tap/observe leave the sim leased+released, so it re-parks; measure booted
# idle CPU by holding a lease (thawed) and sampling.
LEASE=$(curl -fsS -X POST "$DAEMON/v0/leases" -d "{\"udid\":\"$UDID\",\"agent_id\":\"bench-cpu\",\"ttl_seconds\":300}" | /usr/bin/python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
sleep 5
sim_cpu booted_idle 10 2
curl -fsS -X DELETE "$DAEMON/v0/leases/$LEASE" >/dev/null
stop_daemon
# The daemon logs its side of every thaw (the cached-PID SIGCONT alone,
# without lease bookkeeping); fold those into the raw results too.
grep -o 'thaw_ms=[0-9]*' "$DLOG" | cut -d= -f2 \
  | /usr/bin/python3 -c '
import json, sys
vals = [int(l) for l in sys.stdin if l.strip()]
if vals:
    print(json.dumps({"phase": "thaw_daemon_sigcont", "n": len(vals), "ms": vals}))
' >> "$OUT/bench.jsonl"

echo "== phase B: cold actions + boots (daemon --no-warm, no pool)"
DLOG="$OUT/daemon-b.log"
start_daemon --no-warm ${LOAD_FACTOR:+--pool-load-factor "$LOAD_FACTOR"}
sleep 2
$BENCH --phase grant --samples "$SAMPLES"
$BENCH --phase tap --label tap_cold --samples "$SAMPLES"
$BENCH --phase observe --label observe_cold --samples "$SAMPLES"
$BENCH --phase boot --samples "$BOOT_SAMPLES"
stop_daemon

echo
echo "== raw results: $OUT/bench.jsonl"
echo "== hardware:"
sysctl -n machdep.cpu.brand_string 2>/dev/null || true
sw_vers | sed 's/^/   /'
xcodebuild -version 2>/dev/null | head -1
