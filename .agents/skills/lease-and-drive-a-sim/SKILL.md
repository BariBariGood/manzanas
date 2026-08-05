---
name: lease-and-drive-a-sim
description: Drive an iOS simulator through a manzanasd daemon - acquire a lease, boot, dispatch composite/batch actions, observe/wait_for_element, screenshot, and release with reset. Use whenever a task needs to interact with a simulator on a host running manzanasd instead of raw ssh+simctl+AXe.
---

# Lease and drive a simulator

Talk to the daemon over HTTP (default port `:7433`). Every mutating op needs an
active lease; the lease ID is the capability token — never share it.

## 1. Find a target and acquire a lease

```sh
D=http://<mac-tailnet-ip>:7433
curl -s $D/v0/healthz                     # {"ok":true,"version":"v0"}
curl -s $D/v0/targets | jq '.targets[] | {udid,name,state,labels}'
```

Acquire by label (preferred — pins reset policy to a device class, not someone
else's sim). `reset:"erase"` guarantees the NEXT holder gets a clean target:

```sh
L=$(curl -s -X POST $D/v0/leases -d '{
  "labels":["iphone-air"], "agent_id":"<your-session-id>",
  "purpose":"<task>", "ttl_seconds":900, "reset":"erase"}')
LID=$(echo "$L" | jq -r .id); UDID=$(echo "$L" | jq -r .target_udid)
```

- `201` + `state:"active"` → proceed. `202` + `state:"queued"` → poll
  `GET /v0/leases/$LID` until active (polling keeps your queue slot; queued
  leases that go unpolled for 30 min are dropped).
- `409 no_match` → no target carries those labels; re-check `/v0/targets`.
- Renew at ~half TTL for long tasks: `POST /v0/leases/$LID/renew`.
- Interop: other (non-manzanasd) agents coordinate via `/tmp/manzanas_locks/` file
  locks on the Mac — also write `sim-<udid>` there for the lease's duration and
  remove it when done.

## 2. Boot (asynchronous, gated)

```sh
curl -s -X POST $D/v0/targets/$UDID/boot -d "{\"lease_id\":\"$LID\"}"   # 202
# poll until Booted:
curl -s $D/v0/targets | jq -r ".targets[] | select(.udid==\"$UDID\").state"
```

`503 {"code":"overloaded"}` means a host safety gate refused the boot (running
sim cap, load > 2x cores, or <5 GiB free disk) — back off and retry, or use
another host. Do NOT bypass with raw `simctl boot`.

## 3. Act — prefer composite + batch (fewer round trips)

Single action: `POST /v0/actions` with `{"lease_id","kind","payload"}`.
Batch (max 32, strictly ordered, 5-min budget):

```sh
curl -s -X POST $D/v0/actions:batch -d "{
  \"lease_id\":\"$LID\", \"stop_on_error\":true, \"actions\":[
   {\"kind\":\"launch_app\",\"payload\":{\"bundle_id\":\"com.apple.Preferences\",\"terminate_running\":true}},
   {\"kind\":\"wait_for_element\",\"payload\":{\"label\":\"General\",\"timeout_ms\":30000}},
   {\"kind\":\"tap_element\",\"payload\":{\"label\":\"General\"}},
   {\"kind\":\"wait_tree_stable\",\"payload\":{}}]}"
```

- `tap_element` / `type_into_element` are composite: they poll the a11y tree
  for the predicate (`label`/`role`/`value`/`id`, substring unless
  `exact:true`) and tap the match's centre — use them instead of
  observe→compute→tap round trips.
- `wait_for_element` / `wait_tree_stable` are the sync primitives; never
  sleep-and-hope. Both return `408 timeout` when the budget runs out.
- `observe` returns the compacted tree + a stable `hash` (cheap change
  detection). Tap a node at `x + w/2, y + h/2`.
- Errors: `410 lease_expired` (renew earlier next time), `409
  target_not_booted` (boot first), `503 unavailable` (AXe/a11y bridge not
  ready — retry later), `400 bad_request` (bad payload).

## 4. Screenshot — request JPEG + max_dim to keep bytes small

```sh
curl -s -X POST $D/v0/actions -d "{\"lease_id\":\"$LID\",\"kind\":\"screenshot\",
  \"payload\":{\"format\":\"jpeg\",\"max_dim\":800,\"quality\":80}}" \
  | jq -r .result.jpeg_base64 | base64 -d > shot.jpg
```

A full-res PNG is several MB; `jpeg`+`max_dim:800` is tens of KB. With the
journal enabled, `inline:false` skips the base64 in the response — fetch the
capture from the journal artifact instead (see journal-evidence-export skill).

## 5. Release — always, even on failure

```sh
curl -s -X DELETE $D/v0/leases/$LID    # idempotent
```

Release triggers the auto-reset you asked for at acquire (target held, erased,
left Shutdown, then handed to the queue). Don't shut the sim down yourself
first — the daemon owns post-lease state.

## CLI equivalent

`manzanas` (built by `make build`) maps 1:1 onto the protocol:

```sh
manzanas --daemon $D targets
manzanas --daemon $D lease acquire --labels iphone-air --agent me
manzanas --daemon $D screenshot --lease $LID -o shot.png
manzanas --daemon $D --help     # full command list
```
