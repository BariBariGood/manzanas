# Security Policy

## Reporting a vulnerability

Please report vulnerabilities privately via
[GitHub security advisories](https://github.com/BariBariGood/manzanas/security/advisories/new)
("Report a vulnerability" on the Security tab). Do not open a public
issue for security problems. You should receive an initial response
within a few days.

## Supported versions

Only the latest release (v0.x line) receives security fixes. There are
no backports to older tags.

## Deployment model — read this before exposing the daemon

`manzanasd` (and `manzanas-broker`) have **no authentication or TLS in
v0**. The daemon listens on `:7433` — all interfaces — by default, and
is designed to be reached only over trusted, private networks
(localhost, a LAN you control, or an overlay network such as Tailscale
with its own ACLs).

- **Never expose port 7433 (or the broker's 7440) to the public
  internet.** Anyone who can reach the port can lease simulators, run
  actions, read the journal, and pull screenshots.
- For single-machine use, bind to loopback: `manzanasd --addr
  127.0.0.1:7433`.
- For multi-machine use, rely on network-level access control (VPN /
  overlay-network ACLs / firewall rules) — see
  [docs/fleet.md](docs/fleet.md).
- WebSocket origin checks are intentionally not enforced in v0 for the
  same reason; do not rely on them.
