# Element map (compiled flows)

The element map is the daemon's cross-run memory of where named elements
live on screen. Every successful `tap_element` / `type_into_element`
resolution that went through an observing rung (a11y, OCR) records the
element — its frame, role, label, and a DHash fingerprint of the screen
it resolved on — keyed by bundle id, device class
(`<backend>/<img_w>`), and the element spec's canonical matcher string.
Repeated flows (a login sequence, a settings toggle) then compile down
to cache hits: the resolver ladder's **`map` rung runs first** and
serves the cached frame directly, with **no observe round-trip**, when
the current screen's fingerprint is within a small Hamming-distance
threshold of the recorded one.

Results and journal entries carry the provenance: `resolver: "map"` for
a fast-path hit, `"ax"`/`"ocr"` when an observing rung resolved (and the
map re-learned the entry).

## Learning and invalidation

- Learning happens after the ladder resolves an element and before the
  tap, so the fingerprint captures the screen the element was found on.
  It is best-effort and additive: a capture failure just skips the
  recording.
- `refresh: true` bypasses the map rung entirely — the flag asks for a
  cold, uncached observation on every poll, so a remembered frame is
  never served.
- A fingerprint mismatch (the screen drifted) is a silent miss — the
  ladder falls through to the observing rungs, which re-learn the entry
  at its current position. The miss is memoized for the rest of that
  resolution, so the fast path costs at most one screenshot per element
  action.
- A tap failure or focus-guard failure steered by a resolved entry
  invalidates it (removed across all device classes), so the next
  resolution goes back through the observing rungs.

## Storage

Entries live on disk beside the templates dir and the journal's runs
dir — never inside a run dir, so journal GC cannot sweep them:

```
~/.manzanasd/elementmaps/<bundle_id>/<device_class>.json
```

Each file holds one bundle + device class map of matcher string →
element. The device-class key means a frame learned on one capture
geometry never serves another (points vs. capture pixels, different
window sizes).

Daemon flags:

- `--element-map` (default on; env `MANZANASD_ELEMENT_MAP=off`
  disables) — learning and the `map` rung.
- `--element-map-dir` (default `~/.manzanasd/elementmaps`; env
  `MANZANASD_ELEMENT_MAP_DIR`) — the store location; empty disables the
  map.

## Inspecting and pruning

```
manzanas elementmap ls
manzanas elementmap rm BUNDLE_ID NAME [--class axe/390]
```

`ls` maps to `GET /v0/elementmap`; `rm` maps to
`DELETE /v0/elementmap/{bundle_id}?name=...` (all device classes by
default, `--class` scopes one). Both endpoints are leaseless, like the
template library — the map is host-level state (PROTOCOL.md §5.5).
