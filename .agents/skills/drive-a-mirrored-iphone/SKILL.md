---
name: drive-a-mirrored-iphone
description: Drive a physical iPhone through the mirror backend (iPhone Mirroring + mirrord) - which action kinds work, long-press/wheel-scroll, OCR observe semantics (capture_scale, interactable as compatibility constant, interactive_only no-op), coordinate spaces, and testing without a Mac via fakeMirrord and the mock daemon. Use when a task targets a mirror-backed device or extends the mirror action layer.
---

# Drive a mirrored iPhone (mirror backend)

Mirror-backed physical devices are driven by injecting CoreGraphics input
into the iPhone Mirroring window via the `helpers/mirrord` Swift helper
(Unix-socket HTTP/JSON). The Go side lives in
`internal/actions/device_mirror.go` + `internal/actions/mirror/client.go`.

## Coordinate space and scale

- ALL mirror coordinates (tap/swipe/scroll input AND observed frames) are
  **capture-image pixels**, not device points. `observe`/`screenshot`
  results carry `img_w`/`img_h` (the tap coordinate space) and
  `capture_scale` — the capture's pixels-per-window-point factor (≈2.0 on
  a HiDPI/Retina host, 1.0 headless; 0/absent means the helper predates
  the field). Divide by `capture_scale` to get window points.
- Size-capped screenshots still report the pre-transform `img_w`/`img_h`,
  so map detected pixels back through those before tapping.

## Action kinds specific to mirror

- **Long-press**: `tap {x,y,duration_ms}` with `duration_ms` in 1..10000
  (the helper is single-threaded and sleeps the whole press — an
  unbounded press would wedge every later action). Sim (touch-down →
  delay → touch-up) and WDA (W3C pointer chain) implement the same
  field with the same bound.
- **Wheel scroll**: `scroll {x,y,dy}` (`dy` non-zero; positive scrolls
  content down). Mirror-only — it's the trackpad-scroll analog for fine
  list movement without swipe momentum. Sim/WDA return `not_implemented`
  (use `swipe`). Note velocity swipes can't advance paged feeds like
  TikTok (#190) — wheel scroll doesn't either; that needs the
  phone-harness movement work (#196).
- **`scroll` and `type` need focus, everything else doesn't**
  (real-phone findings, #241/#244): `tap`/`swipe`/`button` all land in
  `input_mode: background` with iPhone Mirroring behind other windows,
  but `scroll` fails `unavailable` ("could not be brought frontmost")
  whenever another app holds focus on a shared Mac — fall back to a
  short `swipe`. `type` degrades automatically: background key events
  reach the mirroring process (Cmd shortcuts work) but iOS routes keys
  into text input only while the window is frontmost, so the helper
  briefly activates the window, types, and hands focus back
  best-effort — the ack says `input_mode:"focused-degrade"` with an
  `input_note`. Expect the mirroring window to steal focus for the
  duration of a `type` even in background mode.
- **The iOS on-screen keyboard never appears during `type`** — iPhone
  Mirroring attaches a virtual hardware keyboard, so iOS suppresses the
  soft keyboard by design. A missing keyboard is NOT a focus failure;
  verify typing by the ack's `verify:"ocr-delta"`/pixel delta or by
  re-observing the field.
- CLI: `manzanas tap X Y --duration-ms N`, `manzanas scroll X Y DY`.

## Getting a session started (real phone)

- `GET /v1/status` on the mirrord socket is the health check that
  matters: `state:"blocked"` + `marker:"iphone in use"` means the phone
  is unlocked in someone's hand — mirroring will NOT connect until it
  is locked, and every action returns a screenshot of the "iPhone in
  Use" panel. Poll for `state:"ready"` instead of guessing.
- After a timeout panel, one `tap` on the on-screen **Connect** button
  (≈ centre-x, y≈476 in a 318x701 window) resumes the session.
- Launch apps by Spotlight, not `launch_app` (no devicectl on a mirror
  target): `button home` → `tap` the Search pill → `type "<app>"` →
  `tap` the Top Hit.
- Every input result carries `verified` + `verify_delta` (pixel-delta).
  A `verify_delta` near 0 with `verified:true` means the tap landed but
  the UI didn't change — usually a mis-aimed coordinate; re-screenshot
  before firing more taps.

## Observe = OCR, and its honesty rules

- `observe` runs Vision OCR over a window capture: flat "Text" leaves
  with label+frame only. No roles/ids/values/hierarchy — predicates can
  only match visible text. Icon-only buttons are invisible; fall back to
  coordinate `tap` from a `screenshot`.
- OCR nodes carry `interactable: true` as a uniform **compatibility
  constant** (v0 additivity), NOT a measurement — OCR can't tell a
  button from a caption, so never treat the flag as signal on mirror
  trees. `interactive_only: true` consequently returns the full tree,
  with an explanatory suffix on `note`. Don't rely on that filter on
  mirror devices.
- `wait_for_stable` pixel-diffs captures directly (no OCR) — the cheap
  way to wait out animations; each `wait_tree_stable` poll costs a full
  OCR pass.

## Developing/testing mirror changes without a Mac

- **fakeMirrord** (`internal/actions/device_mirror_test.go`): in-process
  httptest server impersonating mirrord; records `calls`/`body` per
  request, serves canned `boxes`, and `testMirrorBackend(t)` wires it
  into a real `DeviceBackend`. Assert request bodies (e.g. `duration_ms`
  present) and response plumbing (e.g. `capture_scale`) here — this is
  the required e2e smoke path for Go-only slices.
- **Mock daemon**: `manzanasd --mock` + the CLI exercises routing and
  the sim-side `not_implemented` answers (boot the leased mock target
  first or you get `target_not_booted`). Lease acquire returns the ID in
  the `id` field.
- Any `helpers/mirrord` Swift change must at least compile on a real
  Mac: on the Mac mini claim the `build-manzanas` lock, build in
  a throwaway `git worktree` (never touch the checked-out branch or the
  running LaunchAgent), `cd helpers/mirrord && ./build.sh`, then remove
  the worktree and release the lock.

## Protocol discipline

- v0 changes must be ADDITIVE (CONTRIBUTING.md): never reject a
  previously-accepted payload field or drop a response field outright —
  prefer no-op + `note`, or a new optional field. CI review enforces
  this on integration PRs.
- New action kinds/fields go in `proto/PROTOCOL.md` §5 and
  `docs/devices.md` in the same PR.
