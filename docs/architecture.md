# manzanasd architecture

manzanasd is a per-Mac daemon that owns everything stateful about a fleet of
iOS simulators (and, in v0.2, physical devices): enumeration, leases, action
dispatch, media streaming, state fixtures, and the run journal. Clients are
thin and cross-platform.

```
┌ Linux/CI/anywhere ────────────────┐      ┌ each Mac host ──────────────────────────┐
│ manzanas (thin client, Go)         │  WS  │ manzanasd (Go daemon)                     │
│  - CLI: lease/tap/type/observe    │◄────►│  - internal/registry: sims via simctl    │
│  - MCP facade (stdio+HTTP)        │ HTTP │  - internal/lease: TTL, labels, queues   │
│  - stream viewer                  │      │  - internal/server: HTTP+WS protocol     │
└───────────────────────────────────┘      │  - internal/actions: HID/a11y backend    │
        any # of agents + humans           │  - internal/stream: H.264/MJPEG          │
        share leased targets               │  - internal/state: snapshot/fixtures     │
                                           │  - internal/journal: run journal         │
                                           └──────────────────────────────────────────┘
```

## Repository layout

| Path | Owner (v0.1 slice) | Contents |
|---|---|---|
| `cmd/manzanasd/` | foundation | daemon entrypoint (flags/env, wiring, graceful shutdown) |
| `cmd/manzanas/` | client+MCP | thin client CLI + `manzanas mcp` (stdio MCP facade) |
| `cmd/manzanas-broker/` | broker | multi-Mac federation front (see [broker.md](broker.md)) |
| `proto/` | foundation | versioned wire types + `PROTOCOL.md` spec |
| `internal/registry/` | foundation | target enumeration/boot/shutdown/health |
| `internal/lease/` | foundation | in-memory lease table, TTL expiry, FIFO queues, auto-reset/quarantine hooks |
| `internal/server/` | foundation | HTTP+WS server binding all slices to the protocol |
| `internal/warm/` | warm pool | park/thaw pool, footprint watchdog, host safety gates (see [warm-pool.md](warm-pool.md)) |
| `internal/actions/` | actions | HID/a11y action backends: cold AXe + warm simbridge (see [actions-warm.md](actions-warm.md)) |
| `internal/stream/` | stream | MJPEG streamer: capture hubs, HTTP/WS fan-out, view page (see [streaming.md](streaming.md)) |
| `internal/state/` | state | snapshot/fixtures engine + golden-image store (see [state.md](state.md), [images.md](images.md)) |
| `internal/journal/` | journal | run journal: JSONL store, recorder middleware, GC, export (see [journal.md](journal.md)) |
| `internal/broker/` | broker | broker scheduling/health-probe internals |
| `internal/mcp/` | client+MCP | MCP tool implementations behind `manzanas mcp` |
| `internal/client/` | client+MCP | shared HTTP protocol client used by the CLI |
| `eval/` | eval | scenario-driven determinism benchmark (see [eval.md](eval.md)) |
| `helpers/simbridge/` | actions | resident Swift warm-action helper (prebuilt per Mac) |

## Components (foundation)

### proto

The `proto` package is the single source of truth for wire types: `Target`,
`Lease`, `AcquireLeaseRequest`, `ActionRequest`/`ActionResult`,
`StreamRequest`/`StreamOffer`, `JournalRef`, `Error`, and the WS `Envelope`.
Every slice imports it; nothing in `proto` imports any slice. Semantics live
in [`proto/PROTOCOL.md`](../proto/PROTOCOL.md).

### internal/registry

`registry.Registry` abstracts target sources:

```go
type Registry interface {
    List(ctx context.Context) ([]proto.Target, error)
    Get(ctx context.Context, udid string) (proto.Target, error)
    Boot(ctx context.Context, udid string) error
    Shutdown(ctx context.Context, udid string) error
    Health(ctx context.Context, udid string) (proto.TargetState, error)
}
```

Implementations: `SimctlRegistry` (macOS, `xcrun simctl list devices
--json`) and `MockRegistry` (tests + non-macOS dev; the daemon falls back to
it automatically off-macOS). Physical devices arrive in v0.2 as a third
implementation behind the same interface. Labels are derived from runtime +
device type (`ios26`, `ios26.5`, `iphone-17-pro`, `simulator`).

### internal/lease

`lease.Manager` is a concurrency-safe in-memory lease table:

- a lease is a TTL-bounded exclusive claim on one target (default 300s, max
  3600s), with agent id + purpose metadata;
- acquisition matches labels against target label sets; if all matching
  targets are held, the request queues FIFO per label-set with a reported
  queue position;
- a background loop expires overdue leases; release/expiry promotes the
  first queued lease matching the freed target;
- grant/expiry events flow through a `GrantFunc` callback, which the server
  fans out to WS clients (`lease.granted` / `lease.expired`).

### internal/server + cmd/manzanasd

`server.Server` binds registry + leases to the protocol: REST routes under
`/v0/...` and the WS surface at `/v0/ws` (method dispatch mirroring REST,
plus broadcast events). Mutating target ops (boot/shutdown) require an
active lease on the target. Endpoints owned by other slices return
`501 not_implemented` so clients can probe capability.

`cmd/manzanasd` wires it together: `--addr`/`MANZANASD_ADDR` (default
`:7433`), `--mock`/`MANZANASD_MOCK`.

## Interface contracts for the other slices

These are intentionally small and payload-opaque so slices don't collide on
wire types: the core routes opaque payloads; each slice owns its payload
schema. The server validates leases before calling any backend, so backends
receive plain UDIDs.

### actions.Backend (`internal/actions`)

```go
type Backend interface {
    Dispatch(ctx context.Context, udid string, req proto.ActionRequest) (proto.ActionResult, error)
}
```

Wire-in point: `POST /v0/actions` and WS `actions.dispatch`; the server
resolves the target from the request's lease, rejects non-active leases, and
calls `Dispatch`. Backends report protocol-mappable failures as
`*actions.Error` (`actions.WireError` converts one to `proto.Error`); any
other error becomes `internal`.

`AXeBackend` is the v0.1 implementation. It shells out to the AXe CLI
(`~/bin/axe`, or `--axe`/`MANZANASD_AXE`) for HID and accessibility, and to
`xcrun simctl` for app lifecycle and as the screenshot fallback. Every
external command goes through the `actions.Runner` interface, so the unit
tests run on Linux with a fake runner. AXe presence is detected at startup:
when it is missing the daemon still serves, and HID/observe actions return
`unavailable` with an install hint.

One file per concern: `hid.go` (tap/swipe/type/button/key), `observe.go`
(describe-ui + a11y compaction + tree hash), `screenshot.go`, `apps.go`
(install/launch/terminate), with `axe.go` as the kind → handler registry.
`observe` retries `describe-ui` with linear backoff, since it fails with
"No translation object returned" for a moment after an app launch.

A future FBSimulatorControl-native or physical-device backend implements the
same `Backend` interface; nothing outside `internal/actions` knows about AXe.

### stream.Streamer (`internal/stream`) — implemented

```go
type Streamer interface {
    Open(ctx context.Context, udid string, req proto.StreamRequest) (proto.StreamOffer, error)
    Close(ctx context.Context, streamID string) error
}
```

Wire-in point: `POST /v0/streams` and WS `streams.open`, served by
`stream.Manager`. The returned `StreamOffer` carries a WS endpoint
(binary JPEG frames), an HTTP MJPEG endpoint, the browser view page URL,
and the target's current lease holder; N viewers may attach to one
stream. Viewing requires no lease. See [streaming.md](streaming.md).

### state.Engine (`internal/state`)

```go
type Engine interface {
    Snapshot(ctx context.Context, udid, label string) (proto.SnapshotInfo, error)
    Restore(ctx context.Context, udid, snapshotID string, reboot bool) (rebooted bool, err error)
    ListSnapshots(ctx context.Context) ([]proto.SnapshotInfo, error)
    DeleteSnapshot(ctx context.Context, snapshotID string) error
    Erase(ctx context.Context, udid string) error
    Reset(ctx context.Context, udid, spec string) error
    ApplyFixture(ctx context.Context, udid, name string, payload map[string]any) error
}
```

Wire-in point: `/v0/state/...` and WS `state.*` (implemented; see
[`state.md`](state.md)). `Reset` backs the lease manager's per-lease
auto-reset hook (`lease.Manager.SetResetFunc`): when a lease with
`reset: erase|snapshot:<name>` ends, the manager holds the target until
the engine finishes resetting it, then promotes the next queued lease.

### journal.Journal (`internal/journal`)

```go
type Journal interface {
    Append(ctx context.Context, runID string, kind string, payload map[string]any) (proto.JournalRef, error)
    Read(ctx context.Context, runID string, fromSeq int64, limit int) ([]Entry, error)
}
```

Wire-in point: `/v0/journal/...` (implemented; 501 only when the daemon
runs with `--journal-dir ""` — see [journal.md](journal.md));
`ActionResult.journal_ref` carries `proto.JournalRef` values produced by
`Append`.

### warm.Pool (`internal/warm`)

Not a wire slice: the pool wraps the registry (`warm.Guard`) so every
boot passes the host safety gates, thaws parked members on lease grant
(`SetOnActive`), and re-parks them after the post-lease reset. See
[warm-pool.md](warm-pool.md) for lifecycle, watchdog, and flags.

## Design bets

1. **Daemon owns everything stateful; clients are dumb** — the resurrected
   idb-companion model, agent-first.
2. **MCP is a facade, not the core** — the core is the versioned protocol in
   `proto/`; CLI, SDKs, and MCP all sit on that one surface.
3. **Reuse over rewrite** — AXe-style HID techniques for sim input, go-ios
   for device plumbing (v0.2), streaming lessons from serve-sim/iphone-use.
