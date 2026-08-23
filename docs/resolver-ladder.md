# Element resolver ladder

`tap_element` and `type_into_element` resolve their element spec through
an ordered ladder of resolution sources instead of a single hard-wired
observe. One spec (`tap_element {label: "Sign In"}`) works the same on
every backend; what differs is which rungs are registered.

## Rungs

| # | rung | source | registered on |
|---|---|---|---|
| 1 | `map` | element map — cached frames from previously successful resolutions, gated by a screen-fingerprint (DHash) match; no observe round-trip (see [elementmap.md](elementmap.md)) | every backend, when the daemon has an element map (`--element-map`, default on) |
| 2 | `ax` | accessibility tree (AXe on simulators, WDA `/source` on devices) | simulators, WDA-backed devices |
| 3 | `ocr` | OCR text match over the iPhone Mirroring window capture | mirror-backed devices only |
| 4 | `template` | saved-template NCC match over a fresh capture (docs/templates.md) | every backend, when the daemon has a template library |

Each rung attempt is bounded by a per-rung budget (simulator `ax`
~1.5 s, WDA `ax` ~5 s — a `/source` snapshot of a heavy screen
legitimately takes seconds — `ocr` ~3 s) so a dead source — a WDA runner whose snapshot hangs — cannot eat
the whole wait budget before the next rung gets a turn. The per-rung
budget applies only where handing over is possible (multiple observing
rungs, or a breaker-guarded rung); a single unguarded observing rung —
a simulator's `ax` — may use the whole remaining wait budget, so a
legitimately slow cold observe still completes. The `map` rung is
always budget-bounded (a screenshot + a hash) and never counts toward
that single-rung exemption: its miss must hand over promptly. The ladder cycles its rungs until one resolves
the element on screen or `timeout_ms` runs out. A hard source failure
(target not booted, tool missing, mirror blocked) on a single-rung
ladder fails fast with the source's actionable error instead of
retrying until the deadline.

**WDA-only devices have no OCR source**, so the `ocr` rung simply isn't
registered for them; mirror-backed devices have no accessibility tree,
so they ladder OCR → template. The `template` rung (budget ~2 s) keys
the store by the flat matcher's `id` or `label` — as-is when already a
valid template name, plus a slugged form (`"Sign In"` → `"sign-in"`) —
under the launched bundle then `_global`, scoped to the capture's
device class. Structured `predicate` matchers carry no single name and
never hit the template rung. A match at or above the 0.8 NCC threshold
resolves to a synthetic `Template` node covering the matched region;
the tap lands on its centre.

The template rung is **opportunistic**: it checks template existence
(a directory probe) before paying for a capture, and a matcher that
keys no stored template leaves the rung out of the resolve entirely
(trail `template=no-template`). Its presence therefore neither caps a
lone source rung's budget (a simulator's slow cold `ax` observe still
completes) nor keeps a doomed ladder polling — when every source rung
fails hard in a cycle and the template rung cannot serve, the source's
actionable error surfaces immediately.

## Provenance

Successful `tap_element` / `type_into_element` results carry
`resolver`, the name of the rung that resolved the element (`"map"`,
`"ax"`, `"ocr"`, `"template"`, ...), and `resolver_skipped` when a rung was
circuit-skipped (see below). The server lifts these into the journal
entry, so a run's journal shows which source every element resolution
came from.

## AX circuit breaker

WDA's accessibility snapshot dies on hostile apps (TikTok-class), taking
the runner down with it — and every retry repeats the crash. The ladder
guards the `ax` rung on WDA-backed devices with a per-lease,
per-bundle-id circuit breaker: after 3 consecutive **failing action
steps** (runner death, snapshot timeout) for the bundle last launched
on the target, the rung is skipped for the rest of the lease and
results / journal entries carry `resolver_skipped: "ax(circuit-open)"`.
Failures are counted at most once per step, so a transient transport
blip spanning a few polls inside one tap is one failure, not three. A
successful `ax` resolve resets the consecutive count; an open circuit
stays open until the lease ends (a fresh lease re-arms the probe).

## Escalation

When no rung resolves the element within `timeout_ms`, the `408
timeout` is an escalation carrying evidence instead of a bare timeout:

- the error message includes the per-rung outcome trail
  (`ax=no-match, ocr=budget-exceeded`, ...) and the usual off-screen
  candidate hint;
- the journal entry's `escalation` field holds a compact observation
  bundle: `rungs` (the trail), `text_boxes` (up to 24 `{text,x,y,w,h}`
  boxes from the last observation), and any `resolver_skipped`;
- a best-effort screen capture is journaled as an `escalation.png`
  artifact.

The escalating agent gets something to re-plan on — what the screen
looked like and which sources were tried — without another round-trip
to a possibly-dead target.
