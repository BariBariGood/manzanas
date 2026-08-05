# manzanas

A Mac daemon for multi-agent iOS simulator fleet orchestration: leases,
actions, streaming, deterministic state, and an exportable run journal for
AI agents (and humans) sharing simulators.

`manzanasd` runs on each Mac host and owns everything stateful — the
simulator registry, the lease table, the warm pool, action backends,
streamers, golden images, and the run journal. Clients (`manzanas` CLI, MCP
facade, SDKs) are thin and cross-platform, speaking a versioned JSON
protocol over HTTP + WebSocket. `manzanas-broker` federates N daemons
behind one endpoint.

![manzanas CLI demo: two agents lease warm-pool simulators, drive one, and export journal evidence](docs/assets/demo.webp)

*Two agents lease pre-warmed simulators — parked sims thaw on grant instead
of paying a 5 s+ boot — then drive one (warm taps land in ~50 ms end-to-end)
and export the run journal as PR-ready evidence.*

**Website: [manzanasd.vercel.app](https://manzanasd.vercel.app)** (source in
[`site/`](site/)).

## Install

```sh
brew tap baribarigood/tap https://github.com/BariBariGood/homebrew-tap
brew trust baribarigood/tap      # Homebrew >= 6 requires trusting third-party taps
brew install manzanasd            # daemon (+ pulls in the manzanas CLI)
brew services start manzanasd     # launchd service on port 7433
```

See [docs/install.md](docs/install.md) for launchd tarball installs and
alternatives, and [docs/quickstart.md](docs/quickstart.md) for a
single-Mac walkthrough from install to first screenshot.

## Why

Agents driving simulators over raw SSH + CLI tools trip over each other
and pay huge fixed costs. manzanasd removes both, with measured numbers
(M3 Pro, macOS 26.5, Xcode 26.5; reproduce with `make bench`):

- **Leases, not locks**: TTL-bounded exclusive claims with FIFO queues —
  no two agents ever drive the same sim.
- **Park/thaw warm pool**: idle sims are SIGSTOPped (a parked tree is
  unschedulable — ~0 idle host CPU no matter what the sim's daemons are
  doing) and thawed on lease grant: **~0.28 s lease-to-live vs ~7 s for a
  cold boot** (~29 s first boot). The thaw itself is a cached-PID SIGCONT
  and takes under a millisecond.
- **Warm actions**: a resident per-sim helper makes an end-to-end tap
  **~36 ms vs ~950 ms** cold (per-action AXe spawn) — ~3 s cold on Intel.
- **Deterministic state**: snapshots, fixtures, per-lease auto-reset, and
  golden images that stamp out pre-slimmed sims in seconds.
- **Evidence**: every mutating op under a lease is journaled, with
  content-addressed artifacts and a PR-ready markdown export.

| | |
|---|---|
| Leases | TTL-bounded exclusive claims, labels, FIFO queues, auto-reset |
| Warm pool | park/thaw (SIGSTOP) pool sims: ~0.28 s lease-to-live, ~0 idle CPU |
| Actions | cold (AXe) + warm (resident helper) taps/swipes/typing, composite `tap_element`, batches |
| Streaming | MJPEG fan-out, browser `/view` page, WS frames |
| Video | per-lease `simctl` recordings that land in the journal |
| State | snapshots, fixtures, per-lease auto-reset, golden images |
| Journal | append-only evidence per run, artifacts, `export.md` |
| Devices | physical iPhones via devicectl + WebDriverAgent (`--devices`) |
| Fleet | `manzanas-broker` federates N Macs behind one endpoint |
| Clients | `manzanas` CLI, MCP tools over stdio, npm wrapper, GitHub Action |

## Quickstart

New here? [docs/quickstart.md](docs/quickstart.md) walks through a full
single-Mac session (install → lease → tap → screenshot). The short
version:

```sh
make build            # builds bin/manzanasd, bin/manzanas, bin/manzanas-broker

# on a Mac with Xcode:
./bin/manzanasd --addr :7433

# anywhere (Linux/dev/CI), with a mock fleet:
./bin/manzanasd --addr :7433 --mock

# list simulators
curl -s localhost:7433/v0/targets | jq

# lease one by label, boot it, release it
curl -s -X POST localhost:7433/v0/leases \
  -d '{"labels":["ios26"],"agent_id":"me","ttl_seconds":300}' | jq
curl -s -X POST localhost:7433/v0/targets/<udid>/boot -d '{"lease_id":"<id>"}' | jq
curl -s -X DELETE localhost:7433/v0/leases/<id> | jq
```

Or the thin client (`--json` for machine output; see
[clients/](clients/README.md)):

```sh
manzanas targets
manzanas lease acquire --labels ios26 --agent me --wait
manzanas tap 200 400 --lease lse_...
manzanas mcp        # lease-scoped MCP tools over stdio for agents
```

Production installs (launchd, releases): [docs/install.md](docs/install.md).

## Architecture

```
┌ Linux / CI / anywhere ────────────┐        ┌ each Mac host ─────────────────────────────┐
│ manzanas (thin client, Go)         │   WS   │ manzanasd (Go daemon)  :7433                │
│  - CLI: lease/tap/observe/...     │◄──────►│  registry ── warm pool (park/thaw, gates)  │
│  - MCP facade (stdio)             │  HTTP  │  leases (TTL, labels, FIFO, auto-reset)    │
│  - eval harness (manzanas-eval)    │        │  actions ── cold AXe / warm simbridge      │
├───────────────────────────────────┤        │  streams (MJPEG fan-out, browser view)     │
│ manzanas-broker  :7440             │        │  state (snapshots, fixtures, golden images)│
│  fleet-wide placement: leases are │───────►│  journal (evidence, artifacts, export.md)  │
│  scheduled across N daemons, then │ probe/ └────────────────────────────────────────────┘
│  clients talk to the owning       │ lease         × one daemon per Mac in the fleet
│  daemon directly (host_addr)      │
└───────────────────────────────────┘
```

## Documentation

Protocol:

- [proto/PROTOCOL.md](proto/PROTOCOL.md) — the v0 wire protocol: targets,
  leases, actions (incl. composite `tap_element`/`type_into_element` and
  `actions:batch`), streams, state, images, journal, WS surface.

Subsystems:

- [docs/architecture.md](docs/architecture.md) — components and the
  interface contracts between slices.
- [docs/agent-qa.md](docs/agent-qa.md) — a worked end-to-end agent QA
  session, plus driving animated (React Native) apps: screenshot/type
  races, waiting strategy, batch entry shape.
- [docs/devices.md](docs/devices.md) — physical iPhones as leasable
  targets (devicectl + WebDriverAgent).
- [docs/recording.md](docs/recording.md) — per-lease video capture into
  the journal.
- [docs/warm-pool.md](docs/warm-pool.md) — park/thaw warm pool, footprint
  watchdog, host safety gates (503 overloaded).
- [docs/actions-warm.md](docs/actions-warm.md) — warm vs cold action
  paths; the resident `simbridge` helper.
- [docs/streaming.md](docs/streaming.md) — MJPEG streaming and the
  browser view page.
- [docs/dashboard.md](docs/dashboard.md) — the built-in read-only web
  dashboard at `/dash` (fleet, leases, pool, journal browser).
- [docs/state.md](docs/state.md) — snapshots, fixtures, per-lease
  auto-reset, quarantine.
- [docs/images.md](docs/images.md) — golden images: slim once, stamp N
  sims in seconds.
- [docs/journal.md](docs/journal.md) — run journal format, artifacts, GC.
- [docs/broker.md](docs/broker.md) — multi-Mac federation.
- [docs/eval.md](docs/eval.md) — the scenario-driven determinism
  benchmark harness.
- [clients/README.md](clients/README.md) — `manzanas` CLI + MCP
  quickstarts; [clients/npm/](clients/npm/README.md); GitHub Action in
  [action/](action/README.md).

Operations:

- [docs/install.md](docs/install.md) — launchd install, releases,
  Homebrew formula.
- [docs/fleet.md](docs/fleet.md) — running a multi-Mac fleet (topology,
  Tailscale, day-2 ops).
- [docs/troubleshooting.md](docs/troubleshooting.md) — 503 overloaded,
  boot failures, parked-sim semantics, quarantine, and friends.


## Status

Everything above is implemented and running on the fleet — leases +
queues, warm pool with safety gates, cold (AXe) + warm (simbridge) action
backends, composite/batch actions, MJPEG streaming, video capture,
snapshots/fixtures/auto-reset, golden images, journal, dashboard,
physical-device support, broker, eval harness, CLI + MCP. Verify with
`go build ./... && go vet ./... && go test ./...` (Linux-safe; simctl
paths are mocked).

## License

MIT — see [LICENSE](LICENSE).
