# Streaming (v0.1)

The stream slice (`internal/stream`) serves a live view of each booted
simulator's screen to N concurrent viewers. Viewing is read-only and does
not require a lease; the stream offer reports the target's current lease
holder so viewers can see who is driving.

## Capture choice: paced `simctl screenshot`, not `recordVideo`

Two capture paths were considered, following what serve-sim / SimStream and
friends do in this space:

1. **`xcrun simctl io <udid> recordVideo` piped** (what serve-sim uses for
   its H.264 path). It emits an H.264/HEVC **MP4 container**, not an
   elementary stream, and only finalizes the moov atom on SIGINT — so live
   consumption requires either forcing fMP4 fragments (undocumented,
   Xcode-version-dependent) or re-muxing through ffmpeg. It also can't
   change fps, restart cheaply per viewer-driven lifecycle, or survive sim
   reboots without respawn logic.
2. **Paced `xcrun simctl io <udid> screenshot --type=jpeg`** (the MJPEG
   approach used by SimStream's fallback and most sim web viewers): one
   subprocess per frame, JPEG straight from the sim's framebuffer, trivially
   restartable and fps-tunable.

v0.1 ships **(2) only**: MJPEG frames captured at a configurable rate
(default 10 fps, clamp 30), fanned out to viewers over two transports.
H.264 remains reserved in the protocol (`format: "h264"` is rejected with
`bad_request` today); when it lands (v0.2, likely `recordVideo` + fMP4
re-mux) it slots in as another `FrameSource`/transport pair without protocol
changes. Effective capture rate is bounded by `simctl` invocation latency
(~0.8s per frame on a 2017 Intel Mac → ~1 fps; faster hosts do better);
the configured fps is an upper bound, not a promise. The pump does no
per-viewer work, so viewer count doesn't affect capture cost.

## Architecture

```
POST /v0/streams {udid|lease_id, format, max_fps}
        │  (server: validate target, look up lease holder)
        ▼
stream.Manager ── one hub per target ── capture pump (FrameSource)
        │                                   │ JPEG frames
        │                     ┌─────────────┴─────────────┐
        ▼                     ▼                           ▼
  StreamOffer         GET /v0/streams/{id}/mjpeg   GET /v0/streams/{id}/ws
  {stream_id, url,    multipart/x-mixed-replace    binary WS messages
   mjpeg_url,         (browser <img>, curl)        (one JPEG per message)
   view_url, fps,
   holder}
```

- **`FrameSource`** (`source.go`) is the capture abstraction:
  `SimctlSource` (macOS, per-frame `simctl io screenshot`) and `FakeSource`
  (synthesized JPEGs; tests, `--mock`, non-macOS hosts). A `SourceFactory`
  selects per target.
- **`hub`** (`hub.go`) owns one target's pump and its viewers. Frames are
  broadcast non-blocking: a slow viewer drops frames instead of stalling
  capture or its peers, and a viewer that accepts no frame at all for 30s
  is disconnected so it stops occupying a `--stream-max-viewers` slot
  (the per-frame write deadline only covers writes that block; at MJPEG
  bitrates the kernel socket buffer can absorb frames from a client that
  never reads).
- The offer echoes the effective capture rate in `fps`: the requested
  `max_fps` clamped to `--stream-max-fps` (or `--stream-fps` when omitted
  or non-positive).
- Attach errors on both media endpoints use the PROTOCOL §1 JSON envelope:
  `404 not_found`, `429 viewer_limit`, `410 stream_gone`.
- **`Manager`** (`manager.go`) implements the `stream.Streamer` contract.
  One stream exists per target; `Open` is idempotent per UDID, so all
  viewers of a target share one capture pump.

## Lifecycle

- `Open` requires a **Booted** target (`409 target_busy` otherwise): a
  stream on a shutdown sim would accept viewers and then hang while each
  `simctl screenshot` runs out its own ~60s timeout.
- `Open` only negotiates; **capture starts when the first viewer attaches**
  (MJPEG or WS) and **stops after the last viewer detaches** plus a linger
  (`--stream-linger`, default 10s) so a reconnecting viewer doesn't pay a
  cold start. Viewers join/leave freely without affecting the sim or each
  other.
- Once the linger elapses with no viewers, the stream is also unregistered
  (it stops counting against `--stream-max-streams`); the same applies to
  streams that were negotiated but never viewed, and to streams whose
  capture source died. A later `Open` for the target mints a fresh stream.
- Capture failures are retried (up to 5 consecutive errors); if the source
  keeps failing, viewers are disconnected so clients notice and can
  reconnect, which restarts capture.
- `DELETE /v0/streams/{id}` (or daemon shutdown) tears the stream down
  immediately, disconnecting all viewers.

## Limits (daemon flags)

| Flag | Default | Meaning |
|---|---|---|
| `--stream-max-streams` | 8 | concurrently open streams (429 `stream_limit` beyond) |
| `--stream-max-viewers` | 16 | concurrent viewers per stream (429 beyond) |
| `--stream-fps` | 10 | capture rate when the request omits `max_fps` |
| `--stream-max-fps` | 30 | clamp for requested rates |
| `--stream-linger` | 10s | capture keep-alive after the last viewer leaves |

## Browser view page

`GET /view/{udid}` serves a static page (embedded via `go:embed`, no build
toolchain) that fetches target metadata, negotiates a stream via
`POST /v0/streams`, shows the current lease holder, and renders the MJPEG
stream in an `<img>`.
