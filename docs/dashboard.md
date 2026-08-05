# Web dashboard

The daemon serves a built-in fleet dashboard at
`http://<daemon-addr>/dash/`. It ships embedded in the `manzanasd` binary
(no extra install, works offline) and talks to the daemon's own v0 API from
the same origin. Views are always read-only-safe; a small set of
operator controls (boot/shutdown, release, stop recording) can be
disabled with `--dash-readonly`.

## What it shows

- **Fleet tab**
  - Header: daemon health (`/v0/healthz`), pool summary (targets / booted /
    parked counts), and a live/polling indicator.
  - **Targets**: name, runtime, device type, state chip (plus a `parked`
    chip when the target reports `warm: true` — parked in the warm pool,
    see [warm-pool.md](warm-pool.md)), labels,
    UDID, a `rec` chip while a video recording is live, and a "live view"
    button that opens the existing `/view/{udid}` MJPEG page in a new tab.
    The button is disabled for parked or non-booted sims: streaming a
    parked (SIGSTOPped) sim would hang, and viewing never thaws or boots
    anything.
  - **Leases**: agent, target, state chip, purpose, an expires-in countdown,
    and queue position.
- **Multiview tab**: a grid of tiles, one per booted (un-parked, simulator)
  target. Streams are strictly opt-in per tile — there are no always-on
  thumbnails, because every MJPEG stream costs real capture CPU on the
  host. Clicking a tile negotiates a stream (`POST /v0/streams`) and
  attaches its MJPEG URL; clicking again detaches this tile's viewer.
  Streams are shared per target, so the dashboard never issues
  `DELETE /v0/streams/{id}` — that would cut off other viewers of the
  same target (a `/view/{udid}` tab, an agent's stream); once the last
  viewer detaches, the no-viewer linger stops capture. "start all" /
  "stop all" toggle every tile at once. When the daemon's
  `--stream-max-streams` cap is hit the tile shows a clear error instead
  of a stream — stop another tile first. Tiles for targets that shut down
  (or get parked) mid-view are removed on the next refresh.
- **Journal tab**: run list from `/v0/journal`, a paged per-run entry view
  (`from_seq`/`next_seq`), inline screenshot artifacts, and a link to the
  run's `export.md` evidence summary.

## Live updates

The page fetches `/v0/targets` + `/v0/leases` on load and every 5 seconds.
It also opens the daemon's event WebSocket (`/v0/ws`) and re-fetches the
lists whenever a `lease.granted`, `lease.expired`, or `target.state` event
arrives — events are used purely as invalidation signals, so the tables
update within a moment of a lease grant or boot. If the WebSocket drops,
the page reconnects with backoff and the 5-second poll keeps the view
current in the meantime (the header indicator shows `live` vs `polling`).

## Controls (and `--dash-readonly`)

The Fleet tab exposes a deliberately small set of mutating controls, every
one behind a confirm dialog and backed by the `/v0/dash/` endpoints
(documented in [PROTOCOL.md §2.1](../proto/PROTOCOL.md)):

- **boot / shutdown** a *free* target — refused (`409`) when the target is
  held by an active lease (the holding agent owns its lifecycle) or parked
  in the warm pool (the pool owns parked sims). Boots pass the same host
  safety gates (running cap, load, disk) as leased boots, so the dashboard
  can never violate host sim caps.
- **release** a lease — addressed by target UDID since the dashboard never
  holds lease IDs; the lease's post-lease reset still runs.
- **stop rec** — stops a target's live recording; the mp4 is finalized and
  ingested into its run's journal exactly like a normal stop
  (`reason: "dash_stop"`).

Starting `manzanasd` with `--dash-readonly` (env `MANZANASD_DASH_READONLY`)
disables all of the above: the endpoints answer `403 read_only` and the UI
hides the buttons (it probes `GET /v0/dash/config`). Controls default to
**enabled**: the same port always serves the full, unauthenticated
mutating v0 API anyway, so a read-only dashboard is an operator foot-gun
guard for shared viewing, not a security boundary. Automatic per-sim video
thumbnails remain off by design — see the Multiview tab's opt-in tiles and
[streaming.md](streaming.md).

## Trust model

The dashboard has the same (lack of) auth as the rest of the v0 API: none.
Access control is the network boundary — bind the daemon to loopback
(`--addr 127.0.0.1:7433`) or a private/tailnet interface, and never expose
the port to the public internet. `GET /v0/leases`, the journal endpoints,
and the `/v0/ws` event stream all carry full lease objects — including
IDs — and the same port also serves the mutating API, so treat
reachability as full control. See [SECURITY.md](../SECURITY.md).
