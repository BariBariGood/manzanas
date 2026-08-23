# Template library

The template library stores named PNG crops of on-screen elements
("templates") for pixel matching: the `save_template` action plus the
list/delete surface (the storage half), and the matching half —
`tap_template`, `assert_template` named lookups, and the resolver
ladder's `template` rung.

## Where templates live

On disk at `~/.manzanasd/templates` by default — **beside** the journal
directory (`~/.manzanasd/journal`), never inside it, so the journal's GC
never sweeps the library. `--templates-dir` (env
`MANZANASD_TEMPLATES_DIR`) moves it; an empty value disables the library
(`save_template` then answers `503 unavailable`).

Layout:

```
templates/
  <bundle_id>/                    # grouping key; "_global" when omitted
    <name>@<device_class>.png     # the cropped pixels
    <name>@<device_class>.json    # metadata sidecar
```

`device_class` is `"<backend>/<img_w>"` (e.g. `axe/393`, `wda/1179`,
`mirror/318`; `/` is written as `-` in filenames). Pixel templates do not
survive scale changes, so a template only applies to captures from the
same backend at the same capture width. The sidecar carries
`device_class`, `window_px` (full capture size), `crop_rect`,
`source_run`, `min_score_seen` (the lowest NCC score a successful match
of this template has seen — drift bookkeeping updated by the matching
actions), and `created_at`.

## Saving a template

`save_template` is a normal lease-scoped action (PROTOCOL.md §5): it
takes a **fresh** capture through the backend's screenshot path, crops
`rect` (capture-image pixels) out of it, and stores the crop. It works on
simulators, WDA devices, and mirror-backed devices, and journals like any
other action.

```sh
curl -s localhost:7433/v0/actions -d '{
  "lease_id": "'$LEASE'",
  "kind": "save_template",
  "payload": {
    "name": "login-button",
    "bundle_id": "com.example.app",
    "rect": {"x": 120, "y": 610, "w": 150, "h": 44}
  }
}'
```

## Listing and deleting

```sh
manzanas templates ls                      # or GET /v0/templates
manzanas templates rm com.example.app login-button          # all classes
manzanas templates rm com.example.app login-button --class axe/393
```

`rm` maps to `DELETE /v0/templates/{bundle_id}/{name}`
(`?device_class=` scopes one class). Both commands take the usual
`--daemon/--token/--json` flags.

## Matching: `tap_template` and `assert_template`

`tap_template` re-captures the screen each poll, grayscales it and the
template, matches by normalized cross-correlation
(`internal/imgutil.MatchNCC`), and taps the centre of the match once
the score reaches `threshold` (default 0.8). It accepts either an
inline `template_base64` or the `name`/`bundle_id?` of a stored
template (looked up under the capture's device class — save the
template from the same backend you tap on), an optional `rect` search
region, and the usual `timeout_ms`/`interval_ms`. On failure the
`timeout` error reports the best score seen. It is a mutating kind: on
mirror-backed devices it requires the `allow_input` opt-in.

```sh
curl -s localhost:7433/v0/actions -d '{
  "lease_id": "'$LEASE'",
  "kind": "tap_template",
  "payload": {"name": "login-button", "bundle_id": "com.example.app"}
}'
```

`assert_template` matches the same way without tapping (PROTOCOL.md
§5 verification actions).

## The resolver `template` rung

On daemons with a template library, `tap_element` /
`type_into_element` fall back to saved templates when the a11y (or
OCR) rung misses: a template named like the flat matcher's `id` or
`label` (as-is or slugged — `"Sign In"` → `"sign-in"`) under the
launched bundle (then `_global`) for the capture's device class is
NCC-matched against a fresh capture, and a hit is tapped with journal
provenance `resolver:"template"`. See docs/resolver-ladder.md.
