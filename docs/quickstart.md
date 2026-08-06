# Quickstart: one Mac, one simulator

A full first session on a single Mac — install the daemon, lease a
simulator, run an action, and grab a screenshot. No fleet, no VPN, no
extra machines required: everything below talks to `localhost`.

Prerequisites: macOS with Xcode (and at least one iOS simulator runtime)
installed — verify with `xcrun simctl list devices`.

## 1. Install and start the daemon

```sh
brew tap baribarigood/tap https://github.com/BariBariGood/homebrew-tap
brew trust baribarigood/tap      # Homebrew >= 6 requires trusting third-party taps
brew install manzanasd            # daemon (+ pulls in the manzanas CLI)
brew services start manzanasd     # launchd service on port 7433
```

Check it's up:

```sh
curl -s localhost:7433/v0/healthz   # {"ok":true,...}
manzanas --version
```

(Building from source instead: `make build`, then
`./bin/manzanasd --addr :7433`. See [install.md](install.md) for launchd
tarball installs.)

The daemon listens on port 7433. It has no auth in v0, so keep it on
localhost or a private network — see [SECURITY.md](../SECURITY.md).

## 2. See your simulators

```sh
manzanas targets
```

Each simulator is listed with a UDID and labels derived from its device
type and runtime (e.g. `iphone-17-pro`, `ios26`).

## 3. Lease one

Leases are TTL-bounded exclusive claims — while you hold one, nothing
else drives that simulator:

```sh
manzanas lease acquire --labels ios26 --agent me --ttl 600 --wait
```

Note the lease ID (`lse_...`) and target UDID in the output. Boot it if
it isn't already booted:

```sh
manzanas boot <udid> --lease lse_...
```

Boot is asynchronous — the command returns immediately while the
simulator comes up. Poll `manzanas targets` until the state reads
`Booted` (usually a few seconds; ~26 ms when the daemon thaws a
warm-pool sim):

```sh
manzanas targets | grep <udid>
```

## 4. Run an action

```sh
manzanas tap 200 400 --lease lse_...
manzanas type "hello" --lease lse_...
```

Right after a boot the accessibility bridge can take a few extra seconds
to come up — a first action may fail with an AXe “No translation object”
error. Just retry, or use the `wait_tree_stable` action to block until
the UI settles (see [agent-qa.md](agent-qa.md)).

Composite actions can target elements by accessibility label instead of
coordinates — see [agent-qa.md](agent-qa.md) for a worked end-to-end QA
session.

## 5. Take a screenshot

```sh
manzanas screenshot --lease lse_... -o shot.png
open shot.png
```

The status line goes to stderr, so stdout stays clean for piping; `-o -`
writes the raw image bytes to stdout instead of a file
(`manzanas screenshot --lease lse_... -o - > shot.png`). Over the raw
HTTP API the image comes back base64 inside the action result envelope
as `result.png_base64` (see [proto/PROTOCOL.md](../proto/PROTOCOL.md) §5).

## 6. Release

```sh
manzanas lease release lse_...
```

The lease expires on its own at TTL if you forget; `--reset erase` on
acquire gives you a clean simulator every time.

## Next steps

- Everything above is also plain HTTP + JSON on `localhost:7433` — see
  [proto/PROTOCOL.md](../proto/PROTOCOL.md) and the README quickstart
  for the `curl` equivalents.
- `manzanas mcp` exposes the same lease-scoped operations as MCP tools
  over stdio for AI agents ([clients/README.md](../clients/README.md)).
- Multiple Macs behind one endpoint: [broker.md](broker.md) and
  [fleet.md](fleet.md).
- No Mac handy? `manzanasd --mock` runs anywhere (Linux/CI) with a fake
  fleet for client development.
