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

## Supervised usbmux forward (--device-wda-forward)

WDA listens on the device; the host reaches it through a usbmux port
forward. A dead forward is indistinguishable from a dead runner from the
client's side, so the daemon supervises it the same way:

```sh
manzanasd --devices \
  --device-wda 00008130-XXXX=http://127.0.0.1:8100 \
  --device-wda-launch 00008130-XXXX=xctestrun:/path/to/WebDriverAgentRunner.xctestrun \
  --device-wda-forward 00008130-XXXX=8100:8100
# env: MANZANASD_DEVICE_WDA_FORWARD
```

The forward supervisor runs `iproxy <local> <remote> -u <udid>`
(libimobiledevice) — or `pymobiledevice3 usbmux forward` when iproxy is
not installed — as a supervised child: its health probe is "something is
listening on the local port", and it is relaunched with the same capped
backoff as the runner. Both children are killed at daemon shutdown.

## Device onboarding (`manzanas device onboard`)

One command from a paired, trusted phone to a leasable `/v0/targets`
entry with working HID:

```sh
manzanas device onboard 00008130-XXXX --apply
```

It locates a WebDriverAgent checkout (cached under `~/.manzanas/wda`,
shallow-cloned from upstream if missing; override with `--wda-src`),
builds it for the device with `xcodebuild build-for-testing` under a
manzanas-owned bundle id (`--wda-bundle-id`, default `com.manzanas.wda`)
and automatic signing, finds the produced device `.xctestrun`, and
assembles the daemon config: WDA URL + supervised `xctestrun:` launch +
supervised usbmux forward (`--forward`, default `8100:8100`).

With `--apply` it merges that config over the daemon's current one via
`POST /v0/devices` and waits for the phone to appear in `/v0/targets` —
no daemon restart. `--apply` requires the daemon to run with
`--auth-token` (the route spawns host processes, so an unauthenticated
daemon refuses it with `403`); use `--config-out` + SIGHUP on a
tokenless daemon. With `--config-out FILE` it merges into a devices
config file instead (SIGHUP the daemon to pick it up). Without either it
prints the equivalent flags and config JSON.

Headless signing flags (see the next section for why each exists):
`--team` (default `$APPLE_TEAM_ID`), `--keychain`, `--asc-key-path` /
`--asc-key-id` / `--asc-issuer-id` (defaults `$ASC_API_KEY_PATH`,
`$ASC_API_KEY_ID`, `$ASC_API_ISSUER_ID`). `--skip-build` reuses an
existing `.xctestrun` from derived data and just regenerates/applies the
config.

On first install, trust the developer profile on the phone
(Settings → General → VPN & Device Management) — the one step that
cannot be automated.

## Building and signing WDA for a real device, headless

The fallback when `manzanas device onboard` cannot be used. WDA is an
XCUITest bundle and must be signed for a team you control; a stock
checkout built over SSH hits three walls, in this order:

1. **The project's hardcoded team.** Upstream WDA ships someone else's
   `DEVELOPMENT_TEAM` and `PRODUCT_BUNDLE_IDENTIFIER`, so a device build
   fails immediately with `No Account for Team "..."` / `No profiles for
   '....xctrunner' were found`. Override both on the command line —
   never edit the project.
2. **No provisioning profile for `*.xctrunner`.** Automatic provisioning
   must create one; headlessly (no logged-in Xcode) that requires
   `-allowProvisioningUpdates -allowProvisioningDeviceRegistration` plus
   App Store Connect API credentials
   (`-authenticationKeyPath/-authenticationKeyID/-authenticationKeyIssuerID`).
3. **Headless codesign over SSH.** Even with a profile, `codesign` fails
   with `errSecInternalComponent` unless the signing identity's private
   key lives in an **unlocked** keychain whose key partition list
   includes `apple-tool:` (i.e. codesign). Use a dedicated signing
   keychain: import the certificate + key, add it to the search list,
   then `security unlock-keychain -p <pw> <name>.keychain` and
   `security set-key-partition-list -S apple-tool:,apple: -k <pw> <name>.keychain`
   before building.

The full working invocation:

```sh
security unlock-keychain -p <pw> dev-sign.keychain   # if using a dedicated keychain

xcodebuild build-for-testing \
  -project WebDriverAgent/WebDriverAgent.xcodeproj \
  -scheme WebDriverAgentRunner \
  -destination id=<udid> \
  -derivedDataPath ~/.manzanas/wda-derived-<udid> \
  -allowProvisioningUpdates \
  -allowProvisioningDeviceRegistration \
  -authenticationKeyPath ~/.asc_key.p8 \
  -authenticationKeyID $ASC_API_KEY_ID \
  -authenticationKeyIssuerID $ASC_API_ISSUER_ID \
  PRODUCT_BUNDLE_IDENTIFIER=com.manzanas.wda \
  CODE_SIGN_STYLE=Automatic \
  DEVELOPMENT_TEAM=<team-id> \
  'OTHER_CODE_SIGN_FLAGS=--keychain dev-sign.keychain'
```

This produces `WebDriverAgentRunner_iphoneos*.xctestrun` under the
derived-data `Build/Products` directory and installs the runner on the
phone; the runner bundle id becomes `com.manzanas.wda.xctrunner`. Trust
the developer profile on the phone on first run. Then wire it up with
`--device-wda` + `--device-wda-launch xctestrun:<path>` +
`--device-wda-forward <local>:<remote>` (or the config file below), and
the daemon keeps everything running unattended.

## Runtime device config (config file, SIGHUP, POST /v0/devices)

Device enumeration and WDA wiring are runtime-mutable: attaching a phone
never requires killing the daemon. Three equivalent surfaces share one
shape (`proto.DevicesConfig`):

```json
{
  "enabled": true,
  "wda": {
    "00008130-XXXX": {
      "url": "http://127.0.0.1:8100",
      "launch": "xctestrun:/path/to/WebDriverAgentRunner.xctestrun",
      "forward": "8100:8100"
    }
  }
}
```

- **Config file** — `--devices-config /path/devices.json` (env
  `MANZANASD_DEVICES_CONFIG`), or drop a `devices.json` into the state
  dir (`~/.manzanasd/devices.json`) and it is picked up with no flags at
  all. When present, the file replaces the `--devices`/`--device-wda*`
  flag pile (the flags keep working for deployments that use them).
- **SIGHUP** — `kill -HUP $(pgrep -f manzanasd)` re-reads the config
  file and applies it live: supervisors for removed devices stop, changed
  ones restart, added ones start. A file that fails to parse or validate
  is rejected wholesale and the running config is kept.
- **`POST /v0/devices`** — apply a full config over HTTP (`GET
  /v0/devices` returns the current one). The body replaces the whole
  config; merge client-side (as `manzanas device onboard --apply` does)
  if you want add-a-device semantics. Runtime-only: the config file is
  not rewritten, so a later SIGHUP restores the file's view.
  Because the submitted config makes the daemon spawn host processes
  (xcodebuild, iproxy), this route requires the daemon to be started
  with `--auth-token` and returns `403` otherwise — the config file and
  SIGHUP remain the tokenless path.

```sh
# attach a phone to a live daemon (daemon started with --auth-token):
curl -X POST localhost:7433/v0/devices \
  -H "Authorization: Bearer $MANZANASD_AUTH_TOKEN" -d '{
  "enabled": true,
  "wda": {"00008130-XXXX": {"url": "http://127.0.0.1:8100",
           "launch": "xctestrun:/path/to/wda.xctestrun",
           "forward": "8100:8100"}}
}'
# detach everything:
curl -X POST localhost:7433/v0/devices \
  -H "Authorization: Bearer $MANZANASD_AUTH_TOKEN" -d '{"enabled": false}'
```

## Running as a service (`manzanasd --install-service`)

```sh
manzanasd --install-service --devices-config ~/.manzanasd/devices.json --addr :7433
```

Writes (or refreshes) the per-user LaunchAgent
`~/Library/LaunchAgents/com.baribarigood.manzanasd.plist` running the
current binary with the current flags (minus `--install-service`
itself, and with the installing shell's `MANZANASD_*`/`MANZANAS_*`
environment snapshotted into the plist), then `launchctl bootstrap` +
`kickstart`s it. Idempotent: re-run with new flags to update the
service. Per-user domain only — no sudo, logs under
`~/.manzanasd/logs/` (the same files `deploy/install.sh`'s daily
rotation agent trims). A manually copied plist or
a bare `manzanasd &` is **unsupervised**: nothing restarts it after a
crash or reboot; use `--install-service` (or `deploy/install.sh`) for a
supervised daemon.

## The mirror backend (iPhone Mirroring + CGEvents)

Some apps (TikTok and friends) kill WDA: requesting the XCUITest
accessibility snapshot makes the runner crash, taking even coordinate
taps down with it. The `mirror` backend drives the phone through macOS
**iPhone Mirroring** instead — no XCTest on the phone at all. Input is
synthesized CGEvents into the mirroring window, observation is
`screencapture` of that window plus Vision-framework OCR.

Select it per device (default remains `wda`):

```sh
manzanasd --devices \
  --device-backend 00008130-XXXX=mirror \
  --device-mirror-socket ~/.manzanasd/mirrord.sock   # default
# env: MANZANASD_DEVICE_BACKEND, MANZANASD_DEVICE_MIRROR_SOCKET
```

Or select it in the devices config file (loaded at startup, re-read on
SIGHUP, replaceable at runtime via `POST /v0/devices` — see "Runtime
device config" above). Mirror-backed devices live under a
top-level `mirror` key, siblings of `wda`:

```json
{
  "enabled": true,
  "mirror": {
    "00008130-XXXX": { "socket": "/Users/you/.manzanasd/mirrord.sock" }
  }
}
```

`socket` may be omitted (default `~/.manzanasd/mirrord.sock`). A device
is either WDA-backed or mirror-backed; listing a UDID under both
`mirror` and `wda` (or giving a mirror device a
`--device-wda`/`--device-wda-launch` entry) is a validation error. App
lifecycle (`install_app`, `launch_app`, `terminate_app`) stays on
devicectl for both backends.

### What mirror actions look like

| Capability | How | Notes |
|---|---|---|
| `tap`, `swipe` | CGEvent mouse events into the window | coordinates are capture-image pixels — the same space `screenshot`/`observe` report |
| `type` | raw HID keycodes (US layout) | iPhone Mirroring forwards keycodes, not unicode: emoji/non-US glyphs return `bad_request` ("untypable"); `strategy: paste` and `require_focus` return `not_implemented` |
| `button` | keyboard shortcuts | only `home` (Cmd+1). `lock`/volume have no mirroring equivalent → `not_implemented` |
| `screenshot` | `screencapture -l <window>` | DRM-protected video renders **black** in the mirror — a black screenshot of a playing video is expected, not a bug |
| `observe` | `screencapture` + Vision OCR | see fidelity below |
| `pasteboard_get`/`set` | — | `not_implemented` (no reliable programmatic path through the mirror) |

### Reduced observation fidelity — declared, not hidden

`observe` on a mirror device returns OCR-derived **text boxes**, not an
accessibility tree. Every mirror response carries `backend: "mirror"`,
and observation/composite results add `fidelity: "ocr"` so callers can
tell they are looking at OCR, not XCUITest:

- nodes are flat `Text` elements — no roles, ids, values, placeholders
  or hierarchy; element predicates effectively match on visible text
  (`label`/`text`) only;
- `tap_element`, `type_into_element`, `scroll_to_element`,
  `wait_for_element`, `wait_tree_stable` work off those OCR boxes and
  stamp `fidelity: "ocr"` on their results;
- icon-only buttons are invisible to OCR — fall back to coordinate
  `tap` from a `screenshot`;
- `include_raw: true` returns the raw boxes with confidences
  (`raw_format: "vision-ocr-boxes"`).

### The mirror is exclusive global state

iPhone Mirroring shows **one phone per Mac** and its window must be
**frontmost** for input to land. Consequences, enforced and documented:

- at most one device per daemon may be mirror-backed (startup error
  otherwise), so the normal per-device lease already serializes all
  mirror access — leasing the mirror-backed device *is* leasing the
  mirror;
- the helper serves requests strictly serially and the Go client
  serializes calls, so concurrent actions cannot interleave gestures;
- the helper re-activates the mirroring window before every input; if
  another window steals focus mid-gesture, events can still be
  swallowed — agents should avoid fighting a human for the same desktop;
- unlocking the physical iPhone pauses mirroring ("iPhone in Use").
  Actions then fail with an actionable `unavailable` error telling you
  to lock the phone; reconnecting is a physical/GUI action the daemon
  never attempts itself.

### mirrord: the GUI helper (and the TCC story)

CGEvents and `screencapture` require **Accessibility** and **Screen
Recording** TCC grants, and TCC grants attach to the *process context*:
a daemon started over SSH is a different TCC subject than a GUI app, so
manzanasd cannot do this itself. The `mirrord` helper
(`helpers/mirrord/`, Swift, system frameworks only) runs as a **user
LaunchAgent inside the logged-in GUI session** and serves HTTP/JSON on a
local unix socket (`0600`); manzanasd is a plain client.

Install (on the Mac, as the logged-in user — never root):

```sh
cd helpers/mirrord
./install.sh          # builds with swiftc, installs ~/bin/mirrord, loads the LaunchAgent
~/bin/mirrord --doctor  # prints exactly which grant/state is missing
```

One-time manual grants (a human must click these; there is no headless
path by design):

1. **System Settings → Privacy & Security → Accessibility** → add
   `~/bin/mirrord` (enables taps/keystrokes).
2. **System Settings → Privacy & Security → Screen & System Audio
   Recording** → add `~/bin/mirrord` (enables window capture).
3. Open **iPhone Mirroring** and connect the phone (first time: approve
   pairing on the phone; the phone must be locked for mirroring to run).

`--doctor` reports each prerequisite (`granted`/`MISSING` with the exact
Settings pane) and triggers the TCC prompts so the binary appears in the
panes. Rebuilds keep grants because the binary path is stable
(`~/bin/mirrord`).

Uninstall cleanly:

```sh
helpers/mirrord/uninstall.sh   # unloads the LaunchAgent, removes binary + socket + logs
# optionally remove the two TCC entries in System Settings
```

### Honest failure modes

| Symptom | Error | Fix |
|---|---|---|
| iPhone Mirroring not running | `unavailable` (`not-running`) | open the app |
| no phone window | `unavailable` (`no-window`) | connect the phone in the app |
| "iPhone in Use" interstitial | `unavailable` (`blocked`) | lock the physical phone |
| helper not running / socket missing | `unavailable` (mirrord helper unreachable) | `install.sh` / check `launchctl print gui/$UID/com.baribarigood.manzanasd.mirrord` |
| capture fails | `unavailable` (`capture-failed`) | grant Screen Recording to mirrord |
| emoji / non-US characters | `bad_request` (`untypable`) | ASCII only |
| black video in screenshots | *no error* | DRM: expected mirror behavior |

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
