# Mirror backend onboarding, the guided path

*Who this is for: an operator setting up a Mac to drive an
XCTest-hostile app (one that crashes WebDriverAgent) on a real iPhone
through macOS iPhone Mirroring. If your app works under WDA, use the
plain device path in [devices.md](devices.md) instead, it is simpler
and higher-fidelity. The full mirror reference (action semantics, OCR
fidelity, error codes) also lives in [devices.md](devices.md); this
page is only the setup walkthrough.*

The loop below is deliberately doctor-driven: after every step, run

```sh
manzanas doctor --target <udid>
```

and do whatever the failing check's `fix:` line says. The doctor
**probes** capabilities, the running mirrord LaunchAgent takes a real
capture and posts a real no-op event record, instead of reading the
TCC database, because TCC happily says "granted" for grants that no
longer work (and vice versa). When every check is green, input will
actually land; you never have to diagnose a silent no-op tap.

## What you need

- A Mac with a **logged-in GUI session** that nobody is actively using
  while runs are in flight. iPhone Mirroring is one-mirror-per-Mac and
  one focus per desktop: in the `focused` input fallback, a human's
  window can swallow the phone's input (and vice versa). The
  `background` input mode (the default when available) removes most of
  this contention, but a dedicated or mostly-idle Mac is still the
  smooth path.
- An iPhone on the **same Apple ID** (with 2FA) as that Mac, physically
  nearby, that can stay **locked** during runs, unlocking it pauses
  mirroring ("iPhone in Use") until it is locked again.
- Not in the EU (Apple disables iPhone Mirroring there).

## 1. Pair the phone with the host

Even a mirror-only host needs a one-time **CoreDevice pairing** so the
daemon can enumerate the device: connect the phone by cable, tap
*Trust*, and check it appears in
`xcrun devicectl list devices`. Without this the mirror can still
capture and observe, but the target is synthesized as
`iPhone (mirror)` with a `no-devicectl` label and
`install_app`/`launch_app`/`terminate_app` fail (they route through
devicectl).

Then open **iPhone Mirroring** on the Mac and connect the phone (first
time: approve the pairing on the phone, then lock it).

## 2. Install the mirrord helper

manzanasd cannot synthesize input or capture windows itself, TCC
grants attach to the process context, and a daemon started over SSH is
the wrong context. The `mirrord` helper runs as a user LaunchAgent
inside the GUI session:

```sh
cd helpers/mirrord
./install.sh        # builds with swiftc, installs ~/bin/mirrord, loads the LaunchAgent
```

## 3. Grant the two TCC permissions

Both grants go to **`~/bin/mirrord`** (the helper binary, not
manzanasd, not Terminal), and both are manual by design; there is no
headless path:

1. **System Settings → Privacy & Security → Accessibility** → add/enable
   `~/bin/mirrord`, enables taps, swipes, and keystrokes.
2. **System Settings → Privacy & Security → Screen & System Audio
   Recording** → add/enable `~/bin/mirrord`, enables window capture.

If the binary is not yet listed in a pane, run `~/bin/mirrord --doctor`
once, its probes trigger the TCC prompts so the entry appears.

> **Swapping the `~/bin/mirrord` binary invalidates BOTH TCC grants, > re-toggle Accessibility *and* Screen & System Audio Recording off and
> on after every rebuild/reinstall.** macOS keys each grant to the
> binary's code signature, not just its path: after a swap the Settings
> panes keep showing "granted" while synthesized events are silently
> dropped and/or captures come back empty. The probe-based doctor
> catches this, a failing `mirror:<udid>:background_input`,
> `:accessibility`, or `:capture` check right after a rebuild means
> exactly "toggle the grants".

## 4. Wire the device into the daemon

Add a `mirror` entry to the devices config
(`~/.manzanasd/devices.json`, picked up automatically; see
[devices.md](devices.md) for the flag and `POST /v0/devices` forms):

```json
{
  "enabled": true,
  "mirror": {
    "00008130-XXXX": {}
  }
}
```

then `kill -HUP $(pgrep -f manzanasd)`. That is a **read-only** mirror:
`observe`, `screenshot`, and the `wait_*` kinds work; every input kind
returns `403 forbidden`.

### Opting into input (`allow_input`)

The mirror drives a real, personal phone, so input is fail-closed by
default. Turn it on deliberately:

```json
  "mirror": {
    "00008130-XXXX": {"allow_input": true}
  }
```

(or start the daemon with `--device-mirror-allow-input`). The daemon
warns at config apply when a mirror is configured without it, so a
read-only mirror is never a silent surprise.

## 5. Verify with the doctor, then drive

```sh
manzanas doctor --target 00008130-XXXX
```

Green means: the daemon is up, the device is enumerated, the LaunchAgent
answered on its socket, a real capture succeeded, and a real event
record posted. Now:

```sh
manzanas lease acquire --udid 00008130-XXXX --agent me --ttl 600
manzanas screenshot --lease lse_... -o phone.png
manzanas tap 200 400 --lease lse_...       # requires allow_input
```

Coordinates are capture-image pixels (the same space screenshots
report). Observation is OCR text boxes, not an accessibility tree, see "Reduced observation fidelity" in [devices.md](devices.md) for what
that changes.

## Background vs focused input

Every input result reports which mode delivered it (`input_mode`):

- **`background`** (preferred, default when available): events are
  addressed to the iPhone Mirroring process without activating it. The
  Mac's desktop stays where its owner left it; a human can keep working
  in other windows.
- **`focused`** (fallback): the helper
  activates the mirroring window first. If another app wins the focus
  race the action fails cleanly (`focus-lost`) rather than typing into
  the wrong window. The daemon logs a loud warning when a device falls
  back to this mode; `mirrord --doctor` says why background input is
  unavailable.

`scroll` is background-capable only where the helper's scroll-record
symbol resolves (`mirrord --doctor` / `/v1/status` report it as
`background_scroll`). Delivery there is best-effort, the WindowServer
intermittently accepts scroll records that move nothing, so each
background scroll is verified. A focused re-delivery happens only after
an extra late-render check proves that the first scroll did not land;
if that check cannot prove a miss, the helper returns
`scroll-not-delivered` instead of risking a duplicate scroll.

Input is also **verified**: the helper compares window captures before
and after each gesture, and an action that cannot be shown to have
landed fails (`input-not-delivered`) instead of returning ok. See
"Focus-free input, and proof it landed" in [devices.md](devices.md).

## When a human needs the phone or the desktop

Pause the lease instead of fighting for input:

```sh
manzanas lease pause lse_... --reason "human-present"
# ... the human uses the phone / the Mac ...
manzanas lease resume lse_...
```

While paused, actions are refused with `409 lease_paused` (the agent
gets an unambiguous signal, not flaky gestures), but `observe` and
`screenshot` stay available so it can watch and re-plan.

## Common silent-failure history, now loud

Everything in this table used to cost a human round-trip; the doctor or
a typed error now names it:

| Used to happen | Now |
|---|---|
| Missing Accessibility grant: taps return ok, nothing moves | input is verified, `input-not-delivered` / failing `background_input`/`accessibility` doctor probes |
| Doctor read the TCC database and was wrong both ways | doctor probes the running LaunchAgent: real capture + real event record |
| Rebuild silently un-grants Accessibility | failing input probe right after a rebuild → toggle the grant |
| Mirror-only host: phone invisible in `/v0/targets`, no warning | target synthesized with `no-devicectl` label + loud daemon warning at config apply |
| Enumerated phone looked dead (`state: Unknown`, `disconnected`) | mirror-derived liveness: `Booted` + `mirror` label while drivable |
| Stray window over the mirror wedged input | deterministic window selection + `focus-lost`/`capture-failed` errors naming the problem |
