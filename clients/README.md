# manzanas clients — quickstarts

`manzanas` is the thin client for a `manzanasd` daemon running on a Mac host.
Build it anywhere (single static Go binary, cross-platform):

```sh
go build -o manzanas ./cmd/manzanas
export MANZANASD_ADDR=mac-host:7433   # or pass --daemon mac-host:7433
```

## CLI in 60 seconds

```sh
manzanas targets                                           # what's in the fleet
manzanas lease acquire --labels ios26 --agent me --wait    # claim a simulator
export MANZANAS_LEASE=lse_...                              # from the output
manzanas boot UDID --lease $MANZANAS_LEASE                  # boot it
manzanas observe --lease $MANZANAS_LEASE                    # compact a11y tree
manzanas tap 200 400 --lease $MANZANAS_LEASE
manzanas type 'hello' --lease $MANZANAS_LEASE
manzanas screenshot -o shot.png --lease $MANZANAS_LEASE
manzanas lease release $MANZANAS_LEASE                      # hand it back
```

Add `--json` for machine-readable output. Every command maps 1:1 onto the
wire protocol (`proto/PROTOCOL.md`).

The same commands work pointed at a `manzanas-broker` fleet endpoint:
acquire places the lease on some Mac, and every lease-scoped command
(boot, actions, observe, screenshot, streams, state, journal, release)
follows the lease's `host_addr` to the owning daemon automatically — you
never re-point `--daemon` / `MANZANASD_ADDR` mid-run. See
[docs/broker.md](../docs/broker.md).

## MCP for agents

`manzanas mcp` serves lease-scoped MCP tools over stdio: `lease_acquire`,
`lease_release`, `lease_renew`, `targets`, `observe`, `tap`, `swipe`,
`type_text`, `button`, `screenshot` (image content), `app`, `state`,
`record_start`, `record_stop`. Leases acquired in a session are
auto-released when the session ends. Full setup + troubleshooting:
[docs/mcp.md](../docs/mcp.md).

### Claude Code

```sh
claude mcp add manzanas -e MANZANASD_ADDR=mac-host:7433 -- /path/to/manzanas mcp
```

### Cursor (`.cursor/mcp.json`)

```json
{
  "mcpServers": {
    "manzanas": {
      "command": "/path/to/manzanas",
      "args": ["mcp"],
      "env": { "MANZANASD_ADDR": "mac-host:7433" }
    }
  }
}
```

### Codex (`~/.codex/config.toml`)

```toml
[mcp_servers.manzanas]
command = "/path/to/manzanas"
args = ["mcp"]
env = { MANZANASD_ADDR = "mac-host:7433" }
```

## Example agent loop

A typical agent transcript over the MCP tools:

```
agent → lease_acquire {"labels": ["ios26"]}
      ← {"id":"lse_1f2e...","state":"active","target_udid":"5D1C...","expires_at":"..."}
agent → observe {"lease_id": "lse_1f2e..."}
      ← {"hash":"sha256:...","tree":[{"role":"Application","children":[{"role":"Button","label":"Settings","frame":{"x":27,"y":71,"w":60,"h":60}}, ...]}]}
agent → tap {"lease_id": "lse_1f2e...", "x": 57, "y": 101}
      ← {"ok": true}
agent → type_text {"lease_id": "lse_1f2e...", "text": "wifi"}
      ← {"ok": true}
agent → screenshot {"lease_id": "lse_1f2e..."}
      ← (image/png)
agent → lease_release {"lease_id": "lse_1f2e..."}
      ← {"id":"lse_1f2e...","state":"released"}
```

If all matching simulators are busy, `lease_acquire` waits in a FIFO queue
(set `"wait": false` to return the queued lease immediately with its
`queue_position`).
