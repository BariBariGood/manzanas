# Product boundary and non-goals

*Who this is for: contributors and operators deciding whether a feature
belongs in manzanas, whether some piece should be its own repo, or what
the mirror backend will and will not do. Users following the quickstart
don't need this page.*

## The boundary: one orchestration core, transports as backends

The YAML run-spec's value is that the **same spec** runs against any
target, a simulator, a WDA-backed phone, a mirrored phone, with
leases, the journal, and the evidence pipeline shared. Splitting a
transport into its own repo would duplicate the whole orchestration
layer for the sake of one backend, so it stays one repo:

- **Transports stay thin behind the actions interface**
  (`internal/actions.Backend`): AXe/simctl for simulators,
  WebDriverAgent for cooperative apps on devices, mirrord for
  XCTest-hostile apps. Each is a dispatch implementation; none owns
  leases, journaling, runs, or state.
- **mirrord is already the right seam**: a standalone helper binary
  (`helpers/mirrord/`, Swift, system frameworks only) speaking
  HTTP/JSON over a unix socket. manzanas ships an installer for it but
  does not depend on its internals; if the mirror path ever needed a
  separate life, the socket protocol is the boundary.
- **The only genuinely separable audience is the broker** (multi-Mac
  federation, a different operator persona than "one Mac, one
  daemon"). If the repo is ever split, the broker goes first, and not
  before traction justifies double maintenance.

"Should X be its own repo?" defaults to **no** unless X has its own
audience *and* duplicates nothing from the core.

## Non-goals

These are permanent, not "not yet":

- **A generic OCR/CV automation framework.** OCR and pixel-template
  matching exist only as the mirror transport's observation layer (and
  the resolver ladder's fallback rungs), behind the same actions API as
  every other backend. There is no ambition to automate arbitrary
  desktop apps or screens outside the leased-iOS-target model.
- **App-specific botting.** No TikTok/Instagram flows, no engagement
  automation, no per-app playbooks. Hostile apps are a transport *test
  case*, they exercise the no-accessibility-tree path, not a market.
- **Bot-detection evasion.** Input-timing spoofing, fingerprint
  evasion, and anything else designed to make synthetic input look
  human are permanently out of scope.

## The operator's line

manzanas automates **your** devices and accounts, under your
supervision, with an evidence journal. Third-party app terms of service
(TikTok's, for example, expressly bans automated interaction) are the
operator's responsibility; the run journal exists partly so operators
own an auditable record of what their agents did.

## What mirror mode will not do (Apple-hard limits)

The mirror backend is honest about the ceiling iPhone Mirroring imposes
(details and error codes in [devices.md](devices.md)):

- **Fail-closed input**: a mirror-backed device is read-only until the
  operator sets `allow_input: true` (or
  `--device-mirror-allow-input`). Observation works out of the box;
  input on a personal phone is an explicit opt-in, and input that
  cannot be proven delivered fails rather than reporting ok.
- One phone mirrors to one Mac at a time, and one Mac mirrors one
  phone; the phone must be on the same Apple ID with 2FA.
- The phone must stay **locked**, picking it up pauses the session
  ("iPhone in Use"), and resuming is a physical/GUI action the daemon
  never performs itself.
- The host Mac needs a logged-in GUI session, and the smoothest setup
  is a Mac nobody is actively using (see
  [mirror-onboarding.md](mirror-onboarding.md)).
- iPhone Mirroring is unavailable in the EU; DRM-protected video
  renders black in captures; camera, microphone, and Face ID do not
  cross the mirror.
