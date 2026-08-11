# Mock mode (`--mock`)

`manzanasd --mock` runs the daemon anywhere — Linux dev boxes, CI — with
no Mac, no Xcode, and no simulators. It is not just a fake target list:
mock targets carry a **full mock action backend**, so the entire agent
loop (lease → observe → tap_element → type → wait_for_element → audit →
screenshot → release) runs against a deterministic synthetic app screen.

```sh
./bin/manzanasd --addr :7433 --mock
```

On a non-macOS host the daemon falls back to mock mode automatically
(pass `--mock` to silence the warning).

## What is mocked

| Layer | Mock behavior |
|---|---|
| Registry | 3 fake simulators (`MOCK-…`); boot/shutdown flip state instantly |
| Actions | the real action handlers over a synthetic UI tree (below) |
| Screenshots | rendered from the synthetic tree — pixels match describe-ui |
| Streaming | MJPEG/WS frames render the same synthetic UI (`/view`, `/dash`) |
| Video | `MockStarter` produces placeholder recordings |
| State/warm pool | disabled (macOS-only) |

The action backend reuses the production `AXeBackend` handlers —
observe compaction, predicate/matcher DSL, wait loops, composite
element actions, batches, audit checks, screenshot transcoding — with
the AXe/simctl process boundary replaced by an in-process synthetic
app (`internal/actions/mockapp`). What CI exercises through `--mock`
is therefore the same code an agent hits on a real Mac, minus the
simulator itself.

## The synthetic screen

Every mock target shows the same deterministic login screen (390x844
points; the content is taller than the viewport so scrolling is real):

| Element | role / id | Behavior |
|---|---|---|
| "Mock Login" | `StaticText` / `title` | static |
| Username | `TextField` / `username` | tap focuses (keyboard appears); typing edits the value |
| Password | `SecureTextField` / `password` | as above; value reads back masked (`*`) |
| Wi-Fi | `Switch` / `wifi` | tap toggles value `0` ↔ `1` |
| Sign In | `Button` / `sign-in` | shows `status`: "Welcome, <username>!" when both fields are filled, else "Missing credentials" |
| status | `StaticText` / `status` | hidden until Sign In is tapped |
| Reset | `Button` / `reset` | restores the initial state |
| "Mock Footer" | `StaticText` / `footer` | starts below the fold — scroll_to_element / swipe to reach it |
| Keyboard | `Keyboard` / `keyboard` | present while a field has focus (satisfies `require_focus`) |

State transitions are synchronous and deterministic: a tap's effect is
visible in the next observe, so `wait_for_element` has something real
to wait on, and repeated runs produce identical trees, hashes, and
screenshots.

Supported action kinds: everything in PROTOCOL.md §5. Notes:

- `type` / `type_into_element` append to the focused field; both the
  `hid` and `paste` strategies work (`paste` goes through the mock
  pasteboard + Cmd-V chord, like a real simulator).
- `swipe` scrolls the content vertically (clamped); horizontal swipes
  are accepted but move nothing.
- `key` / `key_sequence` / `button` are accepted with no synthetic
  effect.
- `launch_app` resets the screen and returns a fake pid;
  `install_app` / `terminate_app` succeed as no-ops.
- `audit` runs the real checks over the synthetic tree + rendered
  screenshot and returns findings with an annotated image.
- Actions against a leased-but-shutdown mock target return
  `409 target_not_booted`, like the real backends.

## Example: the full loop against --mock

```sh
D=http://localhost:7433
L=$(curl -s -X POST $D/v0/leases -d '{"labels":["ios26"],"agent_id":"demo","ttl_seconds":300}')
LID=$(echo "$L" | jq -r .id); UDID=$(echo "$L" | jq -r .target_udid)
curl -s -X POST $D/v0/targets/$UDID/boot -d "{\"lease_id\":\"$LID\"}"

curl -s -X POST $D/v0/actions:batch -d "{
  \"lease_id\":\"$LID\", \"stop_on_error\":true, \"actions\":[
   {\"kind\":\"type_into_element\",\"payload\":{\"id\":\"username\",\"text\":\"agent\",\"require_focus\":true}},
   {\"kind\":\"type_into_element\",\"payload\":{\"id\":\"password\",\"text\":\"pw\"}},
   {\"kind\":\"tap_element\",\"payload\":{\"label\":\"Sign In\"}},
   {\"kind\":\"wait_for_element\",\"payload\":{\"label\":\"Welcome, agent!\",\"timeout_ms\":5000}},
   {\"kind\":\"audit\",\"payload\":{\"inline\":false}},
   {\"kind\":\"screenshot\",\"payload\":{\"inline\":false}}]}"

curl -s -X DELETE $D/v0/leases/$LID
```

With the journal enabled the audit findings, annotated screenshot, and
screenshots land as run artifacts exactly as on a real host — so e2e
tests of journal evidence also run on Linux.

## Limitations

- One fixed screen; there is no app catalog. `launch_app` resets the
  same screen whatever `bundle_id` it is given.
- No physical-device (WDA) mocking; `--devices` still requires real
  hardware.
- Latency is near-zero: mock runs measure protocol and logic, not
  simulator performance (use a real host + `make bench` for numbers).
