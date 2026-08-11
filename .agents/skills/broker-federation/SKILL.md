---
name: broker-federation
description: Run manzanas-broker to federate multiple manzanasd daemons behind one endpoint, including daemons only reachable through an SSH tunnel. Use when agents need "any matching simulator in the fleet" placement across several Macs.
---

# Broker federation

`manzanas-broker` fronts N daemons (one per Mac) for **placement only**:
target enumeration + lease scheduling. After a grant, ALL target-bound
work (boot, actions, streams, state, recording, journal) goes directly
against the lease's `host_addr` — media and actions never flow through
the broker. The `manzanas` CLI and MCP facade follow `host_addr`
automatically, so you can keep them pointed at the broker address for
the whole lease lifecycle; only raw curl/HTTP clients must re-point
themselves.

## Run it

Cross-platform; typically on a Linux box on the same tailnet:

```sh
make build
bin/manzanas-broker --addr :7440 \
  --host mac1=http://100.64.0.1:7433,intel \
  --host mac2=http://100.64.0.3:7433,arm64

curl -s localhost:7440/v0/healthz        # {"ok":true,"role":"broker","hosts":N}
curl -s localhost:7440/v0/fleet/hosts | jq   # per-host up/targets/active_leases
```

Alternatives: `MANZANAS_BROKER_HOSTS='mac1=100.64.0.1:7433,intel;...'` env
(';'-separated) or `--config fleet.json`
(`{"hosts":[{"name","addr","labels"}]}`). Names/addresses must be unique;
static list — restart to add hosts. Run ONE broker per fleet.

## Tunneled hosts

For a daemon that is not directly reachable from the broker's box (or bound
to localhost), forward it over SSH and register the local end:

```sh
ssh -f -N -L 7439:localhost:7433 <user>@<mac-tailnet-ip>
bin/manzanas-broker --addr :7440 --host mac2tun=http://127.0.0.1:7439
```

Caveat: `host_addr` handed to clients is then `http://127.0.0.1:7439`, which
only resolves on the broker's machine — either run clients there too, or
give each client the same tunnel. Prefer direct tailnet addresses whenever
possible.

## Federated leasing

`POST /v0/leases` on the broker takes the normal `AcquireLeaseRequest`.
Labels may include **host-level labels** (the host name and its configured
extras): `["mac2","ios26"]` pins the lease to one Mac; host labels are
stripped before proxying.

- `201` → active; the Lease carries `host` + `host_addr` — raw HTTP
  clients talk to `host_addr` from now on (the CLI/MCP do it for you).
- `202` → queued on the least-loaded candidate; poll `GET /v0/leases/{id}`
  **through the broker** until active.
- `409 no_match` → no host has a matching target. `503 unavailable` → all
  matching hosts are down (retryable outage, not a label mismatch).

Renew/release/get by lease ID route through the broker to the owning daemon.
A broker restart loses its lease→host table — clients that saved `host_addr`
renew/release against the daemon directly (the CLI/MCP client falls back to
its cached `host_addr` automatically); TTL expiry cleans up the rest.

## Health

Hosts are probed every `--probe-interval` (10s default; `--probe-timeout`
3s). A down host's targets vanish and it gets no new leases; existing leases
on it are untouched. No WS surface and no auth on the broker — tailnet-only,
never expose publicly.
