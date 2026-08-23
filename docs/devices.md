# Physical device targets

*Who this is for: operators adding real iPhones/iPads to a daemon. If
you only drive simulators, you don't need this page. For the guided
mirror setup path, start with
[mirror-onboarding.md](mirror-onboarding.md), this page is the
reference.*

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
simulators, e.g. `["device", "ios26", "ios26.5", "iphone-15-pro"]`, so
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
| `tap_element`, `type_into_element`, `scroll_to_element`, `scroll_until`, `scroll_collect`, `wait_for_element`, `wait_tree_stable` | WebDriverAgent HTTP | same composite semantics as simulators, driven off the WDA source tree; `tap_element`/`type_into_element` resolve through the [resolver ladder](resolver-ladder.md) (a11y rung guarded by a per-lease, per-bundle circuit breaker; WDA-only devices register no OCR rung) |
| `wait_for_stable` | WebDriverAgent HTTP | pixel-diff settle over WDA screenshots, same semantics as simulators |
| `key` / `key_sequence` |, | `not_implemented` (raw HID keycodes are an AXe/CoreSimulator concept WDA does not expose) |
| warm pool, state engine (snapshots/erase), video recording, streaming |, | simulator-only; devices are excluded by construction |

`button` on a device accepts `home`, `lock`, `volume-up`, `volume-down`
(WDA has no `siri`/`apple-pay` press). Unknown action kinds return
`bad_request`; recognized simulator-only kinds return `not_implemented`.

Post-lease auto-reset (`reset: "erase"` / `snapshot:`) is rejected for
device leases at acquire time, there is no `simctl erase` equivalent for
a physical phone.

## WebDriverAgent (HID / observe / screenshot)

`devicectl` has no screenshot or HID input support. Input and observation
on a real device go through
[WebDriverAgent](https://github.com/appium/WebDriverAgent), the on-device
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
5m), so a phone that is simply unplugged is not hammered. When a device
action hits a dead WDA mid-lease (the tunnel dropped, the runner
crashed), the failure returns `unavailable` to the caller AND kicks the
supervisor to relaunch immediately; `wait_for_element`/`wait_tree_stable`
treat transient WDA transport errors as "not yet" and keep polling, so a
brief tunnel blip inside a wait budget heals transparently.

Launch spec forms:

- `devicectl:<runner-bundle-id>`, launches an already-installed
  WebDriverAgentRunner via
  `xcrun devicectl device process launch --terminate-existing`. The
  runner bundle id is usually `<product-bundle-id>.xctrunner`.
- `xctestrun:<path>`, runs
  `xcodebuild test-without-building -xctestrun <path> -destination id=<udid>`
  as a long-lived child of the daemon; it is killed and respawned on
  restart, and killed at daemon shutdown.

Note on `devicectl` launches: launching the xctrunner directly starts
the app UI, but on modern iOS the XCTest harness (and therefore WDA's
HTTP server) generally only comes up when driven by an xcodebuild test
session, so `xctestrun:` is the reliable non-interactive form, and
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
(libimobiledevice), or `pymobiledevice3 usbmux forward` when iproxy is
not installed, as a supervised child: its health probe is "something is
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
`POST /v0/devices` and waits for the phone to appear in `/v0/targets`, no daemon restart. `--apply` requires the daemon to run with
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
(Settings → General → VPN & Device Management), the one step that
cannot be automated.

## Building and signing WDA for a real device, headless

The fallback when `manzanas device onboard` cannot be used. WDA is an
XCUITest bundle and must be signed for a team you control; a stock
checkout built over SSH hits three walls, in this order:

1. **The project's hardcoded team.** Upstream WDA ships someone else's
   `DEVELOPMENT_TEAM` and `PRODUCT_BUNDLE_IDENTIFIER`, so a device build
   fails immediately with `No Account for Team "..."` / `No profiles for
   '....xctrunner' were found`. Override both on the command line, never edit the project.
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

- **Config file**, `--devices-config /path/devices.json` (env
  `MANZANASD_DEVICES_CONFIG`), or drop a `devices.json` into the state
  dir (`~/.manzanasd/devices.json`) and it is picked up with no flags at
  all. When present, the file replaces the `--devices`/`--device-wda*`
  flag pile (the flags keep working for deployments that use them).
- **SIGHUP**, `kill -HUP $(pgrep -f manzanasd)` re-reads the config
  file and applies it live: supervisors for removed devices stop, changed
  ones restart, added ones start. A file that fails to parse or validate
  is rejected wholesale and the running config is kept.
- **`POST /v0/devices`**, apply a full config over HTTP (`GET
  /v0/devices` returns the current one). The body replaces the whole
  config; merge client-side (as `manzanas device onboard --apply` does)
  if you want add-a-device semantics. Runtime-only: the config file is
  not rewritten, so a later SIGHUP restores the file's view.
  Because the submitted config makes the daemon spawn host processes
  (xcodebuild, iproxy), this route requires the daemon to be started
  with `--auth-token` and returns `403` otherwise, the config file and
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
service. Per-user domain only, no sudo, logs under
`~/.manzanasd/logs/` (the same files `deploy/install.sh`'s daily
rotation agent trims). A manually copied plist or
a bare `manzanasd &` is **unsupervised**: nothing restarts it after a
crash or reboot; use `--install-service` (or `deploy/install.sh`) for a
supervised daemon.

## The mirror backend (iPhone Mirroring + CGEvents)

(Setting this up for the first time? Follow the doctor-driven
walkthrough in [mirror-onboarding.md](mirror-onboarding.md); the
sections below are the reference.)

Some apps (TikTok and friends) kill WDA: requesting the XCUITest
accessibility snapshot makes the runner crash, taking even coordinate
taps down with it. The `mirror` backend drives the phone through macOS
**iPhone Mirroring** instead, no XCTest on the phone at all. Input is
synthesized CGEvents into the mirroring window, observation is
`screencapture` of that window plus Vision-framework OCR.

Select it per device (default remains `wda`):

```sh
manzanasd --devices \
  --device-backend 00008130-XXXX=mirror \
  --device-mirror-socket ~/.manzanasd/mirrord.sock \  # default
  --device-mirror-allow-input                          # opt into input; read-only without it
# env: MANZANASD_DEVICE_BACKEND, MANZANASD_DEVICE_MIRROR_SOCKET,
#      MANZANASD_DEVICE_MIRROR_ALLOW_INPUT
```

Or select it in the devices config file (loaded at startup, re-read on
SIGHUP, replaceable at runtime via `POST /v0/devices`, see "Runtime
device config" above). Mirror-backed devices live under a
top-level `mirror` key, siblings of `wda`:

```json
{
  "enabled": true,
  "mirror": {
    "00008130-XXXX": {
      "socket": "/Users/you/.manzanasd/mirrord.sock",
      "allow_input": true
    }
  }
}
```

`socket` may be omitted (default `~/.manzanasd/mirrord.sock`). A device
is either WDA-backed or mirror-backed; listing a UDID under both
`mirror` and `wda` (or giving a mirror device a
`--device-wda`/`--device-wda-launch` entry) is a validation error. App
lifecycle (`install_app`, `launch_app`, `terminate_app`) stays on
devicectl for both backends.

### Fail-closed input gate (`allow_input`)

The mirror drives a **real, personal phone** through the host Mac's GUI
session, so input is opt-in and the default is **fail-closed**: a
mirror-backed device without `allow_input: true` (or
`--device-mirror-allow-input` when using the flag pile) is
**read-only**. Observation kinds, `observe`, `screenshot`,
`wait_for_element`, `wait_tree_stable`, work normally; input kinds, `tap` (including long-press), `swipe`, `scroll`, `type`, `button`,
`tap_template`, and
the composite element actions `tap_element`, `type_into_element`,
`scroll_to_element`, `scroll_until`, `scroll_collect`, return
`403 {"code":"forbidden"}` with a message
naming the fix. The gate follows runtime config changes (`POST
/v0/devices`, SIGHUP) like the rest of the mirror wiring, and the
daemon logs a warning at config apply when a mirror is configured
without `allow_input` so a read-only mirror is never a silent surprise.

> **Migration note (breaking change):** mirror deployments configured
> before `allow_input` existed had input enabled implicitly. On
> upgrade, input actions on those deployments start returning
> `forbidden` until the operator adds `"allow_input": true` to the
> device's `mirror` entry in `devices.json` (then SIGHUP), or adds
> `--device-mirror-allow-input` / `MANZANASD_DEVICE_MIRROR_ALLOW_INPUT=1`
> to the daemon's flags. The startup warning above points at exactly
> this.

### What mirror actions look like

| Capability | How | Notes |
|---|---|---|
| `tap`, `swipe` | CGEvent mouse events into the window | coordinates are capture-image pixels, the same space `screenshot`/`observe` report. `tap` takes an optional `duration_ms` (1–10000) for a long-press (CLI: `tap X Y --duration-ms 800`); simulators (touch-down → delay → touch-up) and WDA devices (W3C pointer chain) implement the same field with the same bound |
| `scroll` | CGEvent wheel events at a point | mirror-only kind: `x`, `y`, `dy` (positive scrolls content down; capture-image pixels). Fine list movement without swipe momentum. `not_implemented` on simulators/WDA (CLI: `scroll X Y DY`). Where the helper's scroll-record symbol resolves (status `background_scroll`), wheel scrolls post on the background path, best-effort: delivery is intermittent, so the helper verifies each scroll and re-delivers a verified miss on the focused path (the result's `input_note` says so); scroll is deterministic when focused. On hosts without the symbol scroll is focused-only, so it is refused whenever another app holds the Mac's focus (`focus-lost`) or `--background=on` forbids the fallback (`input-refused`); the opt-in `fallback: "swipe"` payload field delivers such a refused scroll as the equivalent drag instead, the result stamps `method: "swipe-fallback"`, the swipe geometry (`swipe_start_x/y`, `swipe_end_x/y`), a `note` naming the refusal, and the swipe's ack fields, so journals show what actually ran. Without the opt-in the refusal surfaces unchanged. A background scroll that was accepted but changed nothing under `--background=on` is `scroll-not-delivered`, posted-but-unproven, so it never substitutes a swipe and the scrolling composites count it as a non-move |
| `type` | raw HID keycodes (US layout) | iPhone Mirroring forwards keycodes, not unicode: emoji/non-US glyphs return `bad_request` ("untypable"); `strategy: paste` and `require_focus` return `not_implemented`. Typing needs the window frontmost (iOS routes key events into text input only while iPhone Mirroring holds the Mac's focus), so on a background-mode host `type` degrades per action: the helper activates the window, types, and hands focus back best-effort, the result carries `input_mode:"focused-degrade"` and an `input_note`; `--background=on` refuses it (`input-refused`). The iOS on-screen keyboard never appears under mirroring (a virtual hardware keyboard is attached), that is normal |
| `button` | keyboard shortcuts | only `home` (Cmd+1). `lock`/volume have no mirroring equivalent → `not_implemented` |
| `screenshot` | `screencapture -l <window>` | DRM-protected video renders **black** in the mirror, a black screenshot of a playing video is expected, not a bug |
| `observe` | `screencapture` + Vision OCR | see fidelity below |
| `pasteboard_get`/`set` |, | `not_implemented` (no reliable programmatic path through the mirror) |

### Reduced observation fidelity, declared, not hidden

`observe` on a mirror device returns OCR-derived **text boxes**, not an
accessibility tree. Every mirror response carries `backend: "mirror"`,
and observation/composite results add `fidelity: "ocr"` so callers can
tell they are looking at OCR, not XCUITest:

- nodes are flat `Text` elements, no roles, ids, values, placeholders
  or hierarchy; element predicates effectively match on visible text
  (`label`/`text`) only;
- `tap_element`, `type_into_element`, `scroll_to_element`,
  `scroll_until`, `scroll_collect`, `wait_for_element`,
  `wait_tree_stable` work off those OCR boxes and stamp
  `fidelity: "ocr"` on their results. `tap_element` /
  `type_into_element` resolve through the
  [resolver ladder](resolver-ladder.md)'s `ocr` rung (the only rung a
  mirror device registers, there is no accessibility tree) and their
  results carry `resolver: "ocr"`. The list-scrolling composites
  (`scroll_until` / `scroll_collect`) gesture with a trackpad wheel
  scroll on mirror devices (falling back to a momentum flick when the
  wheel path is unavailable) and detect movement by Jaccard overlap of
  the masked OCR text sets, so they work at OCR fidelity without any
  accessibility tree. A scroll gesture the helper delivered but
  pixel-verified to have changed nothing (`input-not-delivered`, or an
  unverified ack whose check ran) is not an input failure for the
  scrolling composites, including `scroll_to_element`, they re-observe
  and let the ordinary movement check confirm the non-move, so a static
  screen ends in the documented soft end-of-list result;
  `verify: "unavailable"` (delivery unknown) remains a hard error;
- OCR nodes carry `interactable: true` as a **compatibility constant**,
  not a measurement: OCR cannot tell a button from a caption, but the
  v0 wire shape keeps the field (additivity). Don't treat it as signal;
  `observe` with `interactive_only: true` returns the full tree and
  says so in the result's `note`. Predicate ranking is unaffected (the
  interactable term is uniform across an all-OCR tree);
- `wait_for_stable` needs no OCR at all: it diffs window captures
  directly, so it is the cheap way to wait out animations and loads on
  a mirror device (each `wait_tree_stable` poll costs a full OCR pass);
- icon-only buttons are invisible to OCR, fall back to coordinate
  `tap` from a `screenshot`;
- `include_raw: true` returns the raw boxes with confidences
  (`raw_format: "vision-ocr-boxes"`);
- `observe` accepts `ocr_level` (`"fast"` | `"accurate"`) and `rect`
  (`{x,y,w,h}` in capture-image pixels) to tune one recognition pass;
  results report what actually ran: `served_scale` (resolution served
  to recognition relative to the capture; the daemon caps the long
  edge at ~1300px by default, and frames at or below 640px wide are
  never downscaled), `ocr_level`, and `rect` (when requested). Helpers
  predating the options route keep working: a default-only observe
  falls back to the legacy full-resolution pass, while explicitly
  requested options fail `bad_request` rather than being dropped.
  Caveat: interstitial detection ("iPhone in Use" etc.) scans the
  recognized text, so a `rect`-scoped observe can miss interstitial
  text outside the region, if a region-scoped observe is unexpectedly
  empty, re-observe without `rect` before trusting the blank result;
- `observe` and `screenshot` results include `capture_scale`, the
  capture's pixels-per-window-point factor, i.e. the host display's
  backing factor (when the mirrord helper reports it). On a 1x display
  (no Retina/HiDPI, e.g. a headless Mac mini without a HiDPI dummy
  plug) captures are half the resolution and **OCR quality degrades
  noticeably**; check `capture_scale` when text recognition seems
  poor, and prefer a 2x display mode on the host.

### Mirror devices in `/v0/targets`

A mirror-backed device is enumerated even when `devicectl` does not know
it (a host with iPhone Mirroring paired but no CoreDevice pairing): the
registry synthesizes a target from the mirror config, deriving liveness
from the mirrord helper's status (probed with a short cache, using the
helper's light mode, `GET /v1/status?light=1`, no capture+OCR, so
target polling never contends with live gestures on the helper's
strictly serial worker):

- mirrord `ready` → `Booted`, label `mirror`;
- mirrord `blocked` (connect / "iPhone in Use" / paused interstitial) →
  still `Booted` and leasable, plus a `mirror-blocked` label, input
  fails until the interstitial clears. Detecting `blocked` needs the
  full OCR-backed status, so the probe pays for one about once a minute
  (and keeps using it while blocked, so the label holds until the
  interstitial clears): the label can lag the interstitial by up to
  that interval, during which the block still surfaces immediately on
  the action/observe paths;
- mirrord not running / no phone window / socket unreachable →
  `Unknown`, plus `disconnected`.

When `devicectl` *does* enumerate the UDID, its metadata (name, model,
runtime and the derived labels) wins and the mirror supplies liveness;
a down CoreDevice tunnel then surfaces as a `no-tunnel` label instead of
`disconnected`, the mirror keeps working without a tunnel, but
`install_app`/`launch_app`/`terminate_app` route through devicectl and
fail. When `devicectl` does not know the UDID at all, the synthesized
target is named `iPhone (mirror)` and carries a `no-devicectl` label
(same devicectl-routed actions fail), and the daemon logs a loud warning
at config apply.

### The mirror is exclusive global state

iPhone Mirroring shows **one phone per Mac**. Consequences, enforced and
documented:

- at most one device per daemon may be mirror-backed (startup error
  otherwise), so the normal per-device lease already serializes all
  mirror access, leasing the mirror-backed device *is* leasing the
  mirror;
- the helper serves requests strictly serially and the Go client
  serializes calls, so concurrent actions cannot interleave gestures;
- the helper selects the mirroring window deterministically: the
  largest-area on-screen layer-0 window of the iPhone Mirroring
  process, cross-checked against the app's AX standard window when
  readable. `/v1/status` reports the chosen `window_id` (CGWindowID)
  inside `window`, and a capture whose pixel size disagrees with the
  selected window's bounds fails loudly (`capture-failed`) instead of
  letting capture and input silently use different rects;
- input is normally delivered **without taking the Mac's focus** (see
  "Focus-free input" below); only the `focused` fallback re-activates the
  mirroring window before every input, and there a window that steals
  focus mid-gesture can still swallow events, agents should avoid
  fighting a human for the same desktop;
- unlocking the physical iPhone pauses mirroring ("iPhone in Use").
  Actions then fail with an actionable `unavailable` error telling you
  to lock the phone; reconnecting is a physical/GUI action the daemon
  never attempts itself.

### Human preemption: paused leases

When a human needs the phone (or the Mac's desktop) mid-lease, pause the
lease instead of fighting for input:

```sh
manzanas lease pause lse_abc123 --reason "human-present"
# ... the human uses the device ...
manzanas lease resume lse_abc123
```

While paused (`POST /v0/leases/{id}/pause`, additive `paused`/
`paused_at`/`paused_reason` fields on the lease):

- dispatching actions on the lease is refused with
  `409 lease_paused`, the agent gets an immediate, unambiguous signal
  instead of flaky gesture failures;
- the read-only `observe` and `screenshot` kinds stay available, so the
  agent can watch the human's progress and re-plan;
- the lease stays `active` and exclusively holds the target, nothing
  else can be granted the device mid-pause;
- the TTL clock keeps running: the holder must keep renewing a paused
  lease (the renewal grace window applies as usual), so an abandoned
  pause can never hold a target forever.

`POST /v0/leases/{id}/resume` unblocks actions (`lease.paused` /
`lease.resumed` WS events fire on the transitions; both ops are also
WS methods `leases.pause`/`leases.resume`). Pause/resume is explicit and
API-driven in v1, an agent or human tooling calls it. Automatic
human-presence detection (mirrord noticing the human unlocking the phone
or stealing window focus and pausing the lease itself) is future work.

### Focus-free input, and proof it landed

Driving the phone used to mean activating the iPhone Mirroring window for
every tap, which took the Mac away from whoever was using it (and let
their keystrokes land in the phone). The helper now has three input modes
and reports which one ran in every action result as `input_mode`:

- **`background`** (default when available): events are addressed to the
  iPhone Mirroring process directly, so nothing is activated and the
  desktop stays where its owner left it. Mouse gestures use CGS event
  records; keystrokes use the public `CGEvent.postToPid`.
- **`focused-degrade`** (`type` only): a per-action degrade on a
  background-mode host, iOS routes key events into text input only
  while iPhone Mirroring is frontmost, so the helper activates the
  window for the duration of the typing, then hands focus back to the
  previously frontmost app (best-effort; the `input_note` names the
  outcome).
- **`focused`**: the old path, activate the window, post at the HID tap,
  and fail with `focus-lost` if another app wins the race. Used when
  background input is unavailable (older/newer macOS without the
  symbols, no GUI session, `--background=off` / `MIRROR_BACKGROUND=0`)
  and for `scroll` on hosts where the background scroll-record symbol is
  unavailable. When that symbol resolves, scroll uses the background path
  first and only falls back after a verified no-change check. The
  daemon logs a loud warning the first time a device falls back to it;
  the reason appears per action in `input_note`.

Capture is in-process and never activates the window either:
`ScreenCaptureKit` single-frame first, a dlsym'd `CGWindowListCreateImage`
as the fast compatibility path, and the `screencapture` subprocess only
as a last resort. `/v1/status` and `/v1/doctor` report the active backend
as `capture` (`sck`, `cgwindowlist`, `screencapture`, `none`); a backend
pinned with `--capture`/`MIRROR_CAPTURE` that this Mac cannot run reports
`none` rather than pretending to work. A capture whose window is
momentarily unlisted waits for it and then fails `no-window`, it never
activates the app to make it appear, and `/v1/status` never posts a probe
event into the phone (its `input_mode` reads `background?` until an input
or `--doctor` run has demonstrated delivery).

Every input is **verified against the phone**: the helper captures the
window before and after the gesture and requires the touched region to
change, re-observing with a longer settle on a miss (the gesture is posted
exactly once; a background wheel scroll is re-delivered only after an
extra late capture against the pre-gesture frame proves no change, and
the re-delivery is suppressed, reported as a verified success, when
the change simply rendered late). Results carry `verified`, `verify`
(`pixel-delta` / `ocr-delta`, `type` verified by newly recognized text
matching the typed string appearing outside the status bar, stronger
than a pixel diff for a few glyphs; unrelated text changes don't count
/ `off` / `unavailable`) and `verify_delta`; an action that
cannot be shown to have landed fails (`input-not-delivered`) instead of
reporting a success nobody can back up. `verify:"unavailable"`, the
events went out but the check could not run, also fails, worded as
*delivery unknown*: re-observe before retrying anything non-idempotent
like `type`, rather than assuming nothing happened. `--verify off` /
`MIRROR_VERIFY=0` opts out, and then results honestly say
`verified:false, verify:"off"`.

Helper flags and env, all optional:

```sh
mirrord --background auto|on|off   # MIRROR_BACKGROUND=0 forces off
mirrord --verify auto|off          # MIRROR_VERIFY=0 forces off
mirrord --capture sck|cgwindowlist|screencapture   # MIRROR_CAPTURE=<name>
```

`--background on` refuses to silently fall back to the focused path: if
background input is unavailable, actions fail rather than stealing focus.

### mirrord: the GUI helper (and the TCC story)

CGEvents and `screencapture` require **Accessibility** and **Screen
Recording** TCC grants, and TCC grants attach to the *process context*:
a daemon started over SSH is a different TCC subject than a GUI app, so
manzanasd cannot do this itself. The `mirrord` helper
(`helpers/mirrord/`, Swift, system frameworks only) runs as a **user
LaunchAgent inside the logged-in GUI session** and serves HTTP/JSON on a
local unix socket (`0600`); manzanasd is a plain client.

Install (on the Mac, as the logged-in user, never root). The guided
way is `manzanas setup`, it builds, signs, and installs the helpers,
probes which grants are missing, and walks through granting each one
(`--report` to detect without touching anything). The manual way:

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

`--doctor` (and `GET /v1/doctor`, same content as JSON) *probes*
capabilities rather than reading TCC answers, it takes a real capture,
posts a real no-op event record, and reports `{name, ok, detail}` per
check plus the resulting `input_mode`, `capture`, and `verify`, because
the TCC database says "granted" for grants that no longer work and
vice-versa. It still triggers the TCC prompts when a prerequisite of the
focused fallback is missing, so the binary appears in the Settings panes.

Grants survive upgrades only when both the path AND the code signature
stay stable: TCC keys the grant to the signature (an ad-hoc signature
means the cdhash of that exact build), so swapping in an ad-hoc rebuild
at `~/bin/mirrord` silently invalidates both grants even though the
Settings toggles still show on. Both `manzanas setup` and `build.sh`
sign each build with the same identity + identifier
(`com.baribarigood.manzanasd.mirrord`), same-identity rebuilds then
keep their grants, and setup skips the swap when the installed binary
is already the same build. Setup refuses to install unsigned: no
identity in the keychain is an error (unlock the signing keychain, or
force ad-hoc with `--sign-identity -` and re-toggle both grants OFF and
back ON after every swap, [troubleshooting.md](troubleshooting.md)).

Background input needs no extra grant beyond the above: it is the same
Accessibility/Screen Recording story, minus the activation.

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
| another app holds focus (input not sent) | `unavailable` (`focus-lost`) | bring iPhone Mirroring frontmost / leave the Mac's GUI idle |
| helper not running / socket missing | `unavailable` (mirrord helper unreachable) | `install.sh` / check `launchctl print gui/$UID/com.baribarigood.manzanasd.mirrord` |
| capture fails | `unavailable` (`capture-failed`) | grant Screen Recording to mirrord |
| input posted, screen unchanged | `unavailable` (`input-not-delivered`) | retry; check the window is connected and interactive (`mirrord --doctor`) |
| `--background=on` but the background path can't deliver (incl. every `scroll`) | `unavailable` (`input-refused`, nothing was sent) | change mirrord's `--background`/`MIRROR_BACKGROUND` on the host; retrying can't help |
| input works but steals focus | *no error*, `input_mode:"focused"` + daemon warning | `mirrord --doctor` for why background input is unavailable |
| `type` briefly takes focus on a background-mode host | *no error*, `input_mode:"focused-degrade"` + `input_note` | expected: iOS routes key events into text input only while the window is frontmost; focus is handed back best-effort |
| emoji / non-US characters | `bad_request` (`untypable`) | ASCII only |
| black video in screenshots | *no error* | DRM: expected mirror behavior |

## Reconnects mid-lease

A lease pins the target, not the connection: if the CoreDevice tunnel
drops while a lease is active, the lease stays valid. devicectl-backed
actions fail with `unavailable: device not connected`, WDA-backed actions
fail with `unavailable: wda ... failed` (and kick the supervisor), and
both recover on the next action once the device reconnects, no
re-lease needed. `manzanas targets` reflects the drop as
`state: Unknown` + a `disconnected` label on the next listing.

## CLI / MCP ergonomics

```sh
manzanas targets --kind device            # devices only (KIND column shows simulator|device)
manzanas targets --labels device,ios26    # label filtering
manzanas lease acquire --labels device --agent me
manzanas lease acquire --udid 00008130-XXXX --agent me   # pin the exact phone
manzanas tap 100 200 --lease lse_...      # positionals first, flags after
```

The MCP `targets` tool takes an optional `kind` filter, and
`lease_acquire` accepts `udid` to pin a specific device.
