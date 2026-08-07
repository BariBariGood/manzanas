# Physical device targets

manzanasd can enumerate physical iPhones/iPads (paired with the host via
Xcode's CoreDevice stack) as leasable targets alongside simulators. The
feature is off by default; enable it per daemon:

```sh
manzanasd --devices                       # flag
MANZANASD_DEVICES=1 manzanasd              # env var
```

With `--devices` the registry merges `xcrun simctl` (simulators) and
`xcrun devicectl list devices` (physical devices). Device targets appear
in `/v0/targets` with `kind: "device"` and labels derived the same way as
simulators, e.g. `["device", "ios26", "ios26.5", "iphone-15-pro"]` — so
lease label matching works unchanged:

```sh
manzanas lease acquire --labels device --agent me
manzanas lease acquire --labels iphone-15-pro --agent me
```

## Connection state

Devices have no Booted/Shutdown lifecycle. The CoreDevice tunnel state
maps onto `state`:

- tunnel `connected` → `Booted` (usable),
- paired but disconnected → `Unknown`, plus a `disconnected` label.

A paired-but-disconnected device is still enumerated (devicectl serves
cached metadata) and still leasable, but connection-requiring actions fail
with a clean `unavailable: device not connected` error. Booting a device
through `/v0/targets/{udid}/boot` returns an error: bring the tunnel up by
connecting the device over USB or Wi-Fi instead.

## What works today

| Capability | Backend | Status |
|---|---|---|
| discovery, labels, leases | `devicectl list devices` | works, including while disconnected |
| `install_app` | `devicectl device install app` | needs a connected device |
| `launch_app` | `devicectl device process launch` (`terminate_running` → `--terminate-existing`) | needs a connected device |
| `terminate_app` | `devicectl device process terminate --pid` | needs a connected device; payload requires `pid` (devicectl terminates by process id, not bundle id) |
| `tap`, `swipe`, `type`, `button`, `observe`, `screenshot` | WebDriverAgent HTTP | needs a running WDA endpoint (below) |
| `pasteboard_get` / `pasteboard_set` | WebDriverAgent HTTP | `pasteboard_get` requires the WDA app to be foregrounded on iOS 13+ (WDA restriction) |
| `tap_element`, `type_into_element`, `scroll_to_element`, `wait_for_element`, `wait_tree_stable` | WebDriverAgent HTTP | same composite semantics as simulators, driven off the WDA source tree |
| `key` / `key_sequence` | — | `not_implemented` (raw HID keycodes are an AXe/CoreSimulator concept WDA does not expose) |
| warm pool, state engine (snapshots/erase), video recording, streaming | — | simulator-only; devices are excluded by construction |

`button` on a device accepts `home`, `lock`, `volume-up`, `volume-down`
(WDA has no `siri`/`apple-pay` press). Unknown action kinds return
`bad_request`; recognized simulator-only kinds return `not_implemented`.

Post-lease auto-reset (`reset: "erase"` / `snapshot:`) is rejected for
device leases at acquire time — there is no `simctl erase` equivalent for
a physical phone.

## WebDriverAgent (HID / observe / screenshot)

`devicectl` has no screenshot or HID input support. Input and observation
on a real device go through
[WebDriverAgent](https://github.com/appium/WebDriverAgent) — the on-device
XCUITest agent (the same one Appium uses; manzanasd speaks its REST
protocol directly, no Appium server needed). Build and run WDA on the
device, then point the daemon at it:

```sh
manzanasd --devices --device-wda 00008130-XXXX=http://127.0.0.1:8100
# several devices: --device-wda "UDID1=http://127.0.0.1:8100,UDID2=http://127.0.0.1:8101"
# env: MANZANASD_DEVICE_WDA
```

Until a WDA URL is configured for a device, WDA-backed actions on it
return `unavailable` (mirroring how simulator HID degrades when AXe is
missing). Endpoints used: `POST /session`, `POST /session/{id}/wda/tap`,
`/wda/keys`, `/wda/dragfromtoforduration`, `/wda/pressButton`,
`/wda/lock`, `/wda/getPasteboard`, `/wda/setPasteboard`,
`GET /screenshot`, `GET /source`.

`observe` on a device parses the XCUITest XML source into the same
compacted node tree simulators return (`tree` + `hash`), so element
predicates and composite actions work identically. Pass
`include_raw: true` in the observe payload to also get the raw XML
(`raw`, `raw_format: "xcuitest-xml"`).

## WDA lifecycle management (auto-launch / restart)

The daemon can keep a device's WDA alive itself instead of requiring a
manually started runner. Give each device a launch spec alongside its
URL:

```sh
# runner already installed on the phone (recommended):
manzanasd --devices \
  --device-wda 00008130-XXXX=http://127.0.0.1:8100 \
  --device-wda-launch 00008130-XXXX=devicectl:com.example.WebDriverAgentRunner.xctrunner

# or drive it through a prebuilt .xctestrun from this host:
manzanasd --devices \
  --device-wda 00008130-XXXX=http://127.0.0.1:8100 \
  --device-wda-launch 00008130-XXXX=xctestrun:/path/to/WebDriverAgentRunner.xctestrun
# env: MANZANASD_DEVICE_WDA_LAUNCH
```

Each spec starts a supervisor for the daemon's lifetime that probes
`GET /status` (every 10s), launches the runner when WDA is down, waits up
to 90s for readiness, and retries with capped exponential backoff (max
5m) — so a phone that is simply unplugged is not hammered. When a device
action hits a dead WDA mid-lease (the tunnel dropped, the runner
crashed), the failure returns `unavailable` to the caller AND kicks the
supervisor to relaunch immediately; `wait_for_element`/`wait_tree_stable`
treat transient WDA transport errors as "not yet" and keep polling, so a
brief tunnel blip inside a wait budget heals transparently.

Launch spec forms:

- `devicectl:<runner-bundle-id>` — launches an already-installed
  WebDriverAgentRunner via
  `xcrun devicectl device process launch --terminate-existing`. The
  runner bundle id is usually `<product-bundle-id>.xctrunner`.
- `xctestrun:<path>` — runs
  `xcodebuild test-without-building -xctestrun <path> -destination id=<udid>`
  as a long-lived child of the daemon; it is killed and respawned on
  restart, and killed at daemon shutdown.

Note on `devicectl` launches: launching the xctrunner directly starts
the app UI, but on modern iOS the XCTest harness (and therefore WDA's
HTTP server) generally only comes up when driven by an xcodebuild test
session — so `xctestrun:` is the reliable non-interactive form, and
`devicectl:` is best-effort for runner builds that self-start (e.g.
Appium's WDA forks with `USE_PORT` baked in). Try `devicectl:` first; if
`/status` never comes up, use `xctestrun:`.

### One-time manual setup (signing)

WDA is an XCUITest bundle and must be signed for the device's team; this
cannot be done headlessly the first time. One-time, per host + device:

1. Clone [WebDriverAgent](https://github.com/appium/WebDriverAgent) and
   open `WebDriverAgent.xcodeproj` in Xcode; set your development team on
   the `WebDriverAgentRunner` target (automatic signing).
2. `xcodebuild build-for-testing -project WebDriverAgent.xcodeproj -scheme WebDriverAgentRunner -destination id=<udid> -allowProvisioningUpdates`
   — this produces `WebDriverAgentRunner_*.xctestrun` in the derived-data
   `Build/Products` directory and installs the runner on the phone.
3. On first launch, trust the developer profile on the phone
   (Settings → General → VPN & Device Management).
4. WDA listens on the device's port 8100; forward it to the host
   (`pymobiledevice3 usbmux forward 8100 8100`, `iproxy 8100 8100`, or a
   Wi-Fi-reachable device IP) and use that as the `--device-wda` URL.

After that, `--device-wda-launch` keeps it running unattended.

## Reconnects mid-lease

A lease pins the target, not the connection: if the CoreDevice tunnel
drops while a lease is active, the lease stays valid. devicectl-backed
actions fail with `unavailable: device not connected`, WDA-backed actions
fail with `unavailable: wda ... failed` (and kick the supervisor), and
both recover on the next action once the device reconnects — no
re-lease needed. `manzanas targets` reflects the drop as
`state: Unknown` + a `disconnected` label on the next listing.

## CLI / MCP ergonomics

```sh
manzanas targets --kind device            # devices only (KIND column shows simulator|device)
manzanas targets --labels device,ios26    # label filtering
manzanas lease acquire --labels device --agent me
manzanas lease acquire --udid 00008130-XXXX --agent me   # pin the exact phone
manzanas act <lease-id> tap -p '{"x":100,"y":200}'
```

The MCP `targets` tool takes an optional `kind` filter, and
`lease_acquire` accepts `udid` to pin a specific device.
