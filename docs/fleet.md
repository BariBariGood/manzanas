# Fleet operations: multi-Mac onboarding

How to run `manzanasd` across a fleet of Macs so multiple agents (humans,
CI, AI sessions) can share simulators safely.

## Topology

- **One daemon per Mac.** Each Mac host runs exactly one `manzanasd`
  instance which owns all simulators on that host. Do not run two daemons
  against the same CoreSimulator state.
- **Port conventions:** every daemon listens on `:7433`. Clients address a
  specific Mac by host, not port (`http://<tailnet-ip>:7433`). If you must
  co-host a second experimental daemon, use `7434+` and a separate
  `--mock` fleet only.
- Clients (manzanas CLI, MCP facade, GitHub Action) are cross-platform and
  talk to daemons over the tailnet.

## Networking: Tailscale

Put every Mac and every client machine on the same tailnet:

1. On the Mac: install the Tailscale app, sign in, note the `100.x.y.z`
   address (`tailscale ip -4`).
2. On Linux clients/CI:
   `curl -fsSL https://tailscale.com/install.sh | sh && sudo tailscale up --authkey=...`.
3. Verify from the client: `curl -s http://<mac-tailnet-ip>:7433/v0/healthz`.

The daemon binds all interfaces by default; rely on the tailnet ACLs for
access control (v0 has no auth by default — do not expose the port to
the public internet). As defense in depth, `--auth-token` (env
`MANZANASD_AUTH_TOKEN`) on the daemon and broker requires a shared
bearer token on everything except `GET /v0/healthz`:

```sh
# daemon and broker each gate their own API:
manzanasd --auth-token "$TOKEN"
manzanas-broker --auth-token "$TOKEN" --daemon-token "$TOKEN" --host emac=100.64.0.1:7433

# clients:
manzanas --token "$TOKEN" --daemon 100.64.0.1:7440 fleet hosts   # or env MANZANAS_TOKEN
```

The broker authenticates to its daemons with `--daemon-token` (env
`MANZANAS_BROKER_DAEMON_TOKEN`) or a per-host `"token"` in the config
file's host entries. Keep one shared token across broker and daemons
whenever the bundled clients are in play: the broker dashboard's live
view presents the browser's token directly to each daemon, and the CLI
/ MCP clients follow a lease's `host_addr` to the owning daemon with
the same `--token` they used for the broker — a split token gets every
lease-scoped call (boot, actions, streams, journal, release) rejected
with 401 by the daemon. Split broker/daemon tokens only work for raw
API clients that talk to the broker alone. A config file carrying
per-host tokens holds credentials in plaintext — keep it `chmod 600`.

The broker's per-host `build` column in
`/v0/fleet/hosts` (and its dashboard) makes daemon version skew visible
during rolling upgrades.

## SSH onboarding (per new Mac)

1. System Settings → Sharing → **Remote Login** ON.
2. Append the fleet's shared public key to `~/.ssh/authorized_keys`.
3. Optional but recommended: passwordless sudo for the fleet user, and
   `sudo pmset -a sleep 0 displaysleep 0` so the Mac never sleeps mid-run.
4. Install Xcode + simulator runtimes; verify `xcrun simctl list devices`.
5. Install the daemon: copy a release tarball over and run
   `deploy/install.sh` (see [install.md](install.md)). The LaunchAgent
   restarts the daemon on crash and on login.

This doc covers the manzanasd-specific parts of running a multi-Mac
fleet.

## Lock protocol (interop with non-manzanasd agents)

Leases are the source of truth *between manzanasd clients*. But other
automation (manual SSH sessions, older agents) may also touch simulators.
The fleet convention is file locks in `/tmp/manzanas_locks/` on each Mac:

- One file per resource: `sim-<udid>`, `device-<udid>`, `build-<repo>`,
  `machine` (exclusive machine-level ops).
- Contents: `<session-id> <ISO-8601 timestamp> <task description>`.
- Locks older than 2 hours are stale and may be removed.
- Before touching a simulator outside a lease, check
  `ls /tmp/manzanas_locks/`, `ps aux | grep -E "[x]codebuild|[S]imulator"`,
  and `xcrun simctl list devices | grep Booted`.
- Never kill processes or shut down simulators you did not start.
- Prefer an atomic helper (ln-based claim) over hand-rolled echo/rm so
  concurrent claims cannot race.

manzanasd's lease table and the file locks will converge in a later
version (the daemon can mirror leases into `/tmp/manzanas_locks/`); until
then, agents driving sims *without* a manzanasd lease must take the file
lock, and manzanasd operators should treat foreign lock files as leased.

## Day-2 operations

- **Health:** `curl -s http://<host>:7433/v0/healthz` per Mac; wire into
  any uptime checker on the tailnet.
- **Logs:** `~/.manzanasd/logs/manzanasd.{out,err}.log`, copy-truncated
  daily (when over 10MB) by the `com.baribarigood.manzanasd.logrotate`
  LaunchAgent that install.sh sets up; one gzipped generation is kept.
- **Upgrade:** re-run `deploy/install.sh --binary <new manzanasd>`; it
  replaces the binary and reloads the LaunchAgent in place.
- **Remove:** `deploy/uninstall.sh [--purge]`.
- **Version skew:** daemons and clients speak the versioned `/v0` protocol;
  pin the `manzanas` CLI to the same release tag as the daemon fleet and
  upgrade daemons first (protocol changes are additive within v0.x).
