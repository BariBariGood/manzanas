import AppKit
import CoreGraphics
import Foundation

/// mirrord — the manzanasd iPhone Mirroring helper.
///
/// A small GUI-session process that drives macOS iPhone Mirroring with
/// synthesized input, in-process window capture (eyes), and Vision OCR
/// (the poor man's element tree). manzanasd talks to it over a local
/// unix socket; mirrord is the process that holds the Accessibility +
/// Screen Recording TCC grants, which is why it must run as a
/// LaunchAgent in the logged-in user's GUI session — a daemon started
/// over SSH is a different TCC subject and gets nothing.
///
/// Protocol: HTTP/1.1 + JSON over the unix socket. Input coordinates
/// are capture-image pixels (the space /screenshot and /ocr report);
/// mirrord converts to Mac screen points internally.
///
/// Input is focus-free where the host allows it (CGS event records
/// addressed to the mirroring process; see BackgroundInput), so driving
/// the phone does not steal the Mac's focus, and every action is
/// answered with evidence: a capture taken before and after the gesture
/// must differ in the region it touched, or the action fails with
/// "input-not-delivered" rather than a hopeful ok:true.

let defaultSocket = NSString(string: "~/.manzanasd/mirrord.sock").expandingTildeInPath

/// Capture scale (image px per window point) measured by the most
/// recent capture. Inputs prefer the live per-display factor below;
/// this cache is the fallback and also feeds /v1/scroll's pixel delta.
/// Declared before any flag handling because --doctor captures too.
var lastScale: Double = Double(NSScreen.main?.backingScaleFactor ?? 2.0)

func usage() -> Never {
    FileHandle.standardError.write("""
    usage: mirrord [--socket <path>] [--background auto|on|off] [--verify auto|off]
                   [--capture sck|cgwindowlist|screencapture] [--doctor]
      --socket      unix socket to serve on (default \(defaultSocket))
      --background  focus-free input: auto (probe, fall back to focused),
                    on (never fall back), off (always activate the window).
                    MIRROR_BACKGROUND=0 is the same as --background off
      --verify      per-action capture-delta verification (default auto).
                    MIRROR_VERIFY=0 is the same as --verify off
      --capture     pin a capture backend (default: best available).
                    MIRROR_CAPTURE=<name> does the same
      --doctor      print capability probes and exit

    """.data(using: .utf8)!)
    exit(2)
}

/// Flags accept both "--flag value" and "--flag=value".
func toggleValue(_ raw: String) -> MirrorConfig.Toggle {
    guard let t = MirrorConfig.Toggle(rawValue: raw.lowercased()) else { usage() }
    return t
}

var socketPath = defaultSocket
var doctor = false
var config = MirrorConfig.fromEnvironment()
var args = Array(CommandLine.arguments.dropFirst())
while !args.isEmpty {
    let arg = args.removeFirst()
    let name = arg.contains("=") ? String(arg.prefix(while: { $0 != "=" })) : arg
    let inline = arg.contains("=") ? String(arg.drop(while: { $0 != "=" }).dropFirst()) : nil
    func value() -> String {
        if let inline = inline { return inline }
        guard !args.isEmpty else { usage() }
        return args.removeFirst()
    }
    switch name {
    case "--socket":
        socketPath = value()
    case "--background":
        config.background = toggleValue(value())
    case "--verify":
        config.verify = toggleValue(value())
    case "--capture":
        let v = value()
        guard Capture.backends.contains(where: { $0.name == v }) else { usage() }
        config.captureBackend = v
    case "--doctor":
        doctor = true
    default:
        usage()
    }
}
MirrorConfig.shared = config

if doctor { Doctor.printText() }

/// Scale for the display actually hosting the mirroring window, so an
/// input issued before any capture (or after the window moves between
/// displays with different densities) still converts correctly.
/// CGWindow bounds are global top-left-origin; NSScreen frames are
/// bottom-left-origin, so flip y before hit-testing.
func scaleFor(_ win: MirrorWindow.Window) -> Double {
    guard let primary = NSScreen.screens.first else { return lastScale }
    let cx = win.x + win.w / 2
    let cy = primary.frame.maxY - (win.y + win.h / 2)
    for s in NSScreen.screens where s.frame.contains(CGPoint(x: cx, y: cy)) {
        return Double(s.backingScaleFactor)
    }
    return lastScale
}

func toScreen(_ x: Double, _ y: Double, _ win: MirrorWindow.Window) -> (Double, Double) {
    let scale = scaleFor(win)
    return (win.x + x / scale, win.y + y / scale)
}

/// Rejects capture-image coordinates outside the mirroring window: a
/// synthesized click past the phone image would land on the host Mac's
/// own desktop/windows (mirrord holds the Accessibility grant, so its
/// events go anywhere on screen).
func checkBounds(_ x: Double, _ y: Double, _ win: MirrorWindow.Window) throws {
    let scale = scaleFor(win)
    let maxX = win.w * scale
    let maxY = win.h * scale
    // Half-open: valid capture pixels are [0, maxX)/[0, maxY) — x == maxX
    // already maps one point past the window's right edge.
    if x < 0 || y < 0 || x >= maxX || y >= maxY {
        throw MirrorWindow.HelperError(
            code: "bad-request",
            message: "point (\(x), \(y)) is outside the mirroring window capture (\(Int(maxX))x\(Int(maxY)) px)")
    }
}

/// The only key combos mirrord will synthesize: the iPhone Mirroring
/// app shortcuts. Anything else (cmd+q, cmd+w, ...) would drive the
/// host Mac, not the phone.
let allowedCombos: Set<String> = [
    "cmd+1", // Home Screen
    "cmd+2", // App Switcher
    "cmd+3", // Spotlight
]

func num(_ body: [String: Any], _ key: String) -> Double? {
    guard let v = (body[key] as? NSNumber)?.doubleValue, v.isFinite else { return nil }
    return v
}

/// Request-supplied durations go straight into Int conversions and
/// sleeps: reject NaN/inf and clamp to 10s so a bad value can neither
/// trap the process nor wedge the serial worker.
func clampMs(_ v: Double?, default def: Int) -> Int {
    guard let v = v, v.isFinite else { return def }
    return Int(min(max(v, 0), 10_000))
}

/// Press/drag acks echo the duration that actually ran. An out-of-range
/// request is clamped rather than rejected (the daemon already 400s it,
/// so only direct socket callers hit the clamp), and the clamp is called
/// out in `note` instead of being applied silently.
func stampDuration(_ out: inout [String: Any], requested: Double?, applied: Int) {
    out["duration_ms"] = applied
    guard let r = requested, r < 0 || r > 10_000 else { return }
    let note = "duration_ms \(String(format: "%.0f", r)) clamped to \(applied)"
    out["note"] = (out["note"] as? String).map { "\($0); \(note)" } ?? note
}

/// Rejects a capture whose pixel size disagrees with the window bounds
/// it was supposedly taken from: capture and input would then be using
/// different rects (the wrong-window failure mode), and every
/// coordinate derived from the image would be wrong. 25% tolerance
/// absorbs Retina rounding and title-bar cropping differences between
/// the window and region capture paths.
func checkCaptureSize(imgW: Int, win: MirrorWindow.Window) throws {
    let expected = win.w * scaleFor(win)
    guard expected > 0 else { return }
    let ratio = Double(imgW) / expected
    if ratio < 0.75 || ratio > 1.25 {
        throw MirrorWindow.HelperError(
            code: "capture-failed",
            message: "capture width \(imgW)px does not match window \(Int(win.w))pt (id \(win.id)) at scale \(scaleFor(win)) — capture and window selection disagree")
    }
}

/// Parses /v1/ocr POST-body options; empty body = legacy defaults.
func ocrOptions(_ body: [String: Any]) throws -> OCR.Options {
    var opts = OCR.Options()
    if let level = body["level"] {
        guard let s = level as? String, s == "fast" || s == "accurate" else {
            throw MirrorWindow.HelperError(
                code: "bad-request", message: "level must be \"fast\" or \"accurate\"")
        }
        opts.level = s == "fast" ? .fast : .accurate
        opts.levelName = s
    }
    if let rect = body["rect"] {
        guard let d = rect as? [String: Any],
              let x = num(d, "x"), let y = num(d, "y"),
              let w = num(d, "w"), let h = num(d, "h"), w > 0, h > 0 else {
            throw MirrorWindow.HelperError(
                code: "bad-request",
                message: "rect requires numeric x, y and positive w, h in capture pixels")
        }
        opts.rect = CGRect(x: x, y: y, width: w, height: h)
    }
    if let maxDim = body["max_dim"] {
        guard let v = (maxDim as? NSNumber)?.doubleValue, v.isFinite, v >= 1 else {
            throw MirrorWindow.HelperError(
                code: "bad-request", message: "max_dim must be a positive integer")
        }
        // Clamp before Int conversion: Int(Double) traps outside Int64
        // range, and no capture needs a cap beyond 100k px anyway.
        opts.maxDim = Int(min(v, 100_000))
    }
    return opts
}

/// Fraction of sampled pixels in the touched region that must change
/// for an action to count as delivered. Deliberately small: a tap that
/// only highlights a row still moves ~1% of a 64x64 sample of that row,
/// while mirrored-video noise sits far below it.
let verifyChangedFraction = 0.005

/// Whole-frame checks (typing, shortcuts, scroll) get a finer grid and a
/// proportionally smaller threshold: a few glyphs appearing in a text
/// field are a large change to a handful of 256x256 cells but round to
/// nothing at 64x64 over a ~1200x2600 frame, which would fail input that
/// was in fact delivered. Each counted cell still has to move more than
/// the per-pixel noise threshold, so ~33 of 65_536 cells is a real
/// change, not stream noise.
let verifyWholeFrameGrid = 256
let verifyWholeFrameChangedFraction = 0.0005

/// How long to wait for the phone to render the consequence of a gesture
/// before each "after" sample. The gesture is posted exactly once; a miss
/// is answered by observing again (mirrored video renders a frame late,
/// slow animations), never by re-posting — a replayed tap or retyped text
/// would be a duplicated action on the phone.
let verifySettleIntervals: [TimeInterval] = [0.25, 0.5, 0.75]

/// Whether a set of newly recognized strings is evidence that `expect`
/// was typed. A new line either contains the typed text (a field showing
/// "existing content" + the new glyphs, or the whole string) or is a
/// *substantial* piece of it — at least half of the typed run and never
/// under 4 characters (the field re-rendered mid-word, a line break
/// inside the typed run). The length floor matters: the on-screen
/// keyboard sliding in during a type action adds dozens of one- and
/// two-character key labels ("q", "123"), all substrings of most typed
/// text, and none of them proof that a keystroke landed. Banners and
/// animation labels don't match either direction. Callers gate on
/// `ocrVerifiable` first; runs it rejects never reach here.
func textAdditionMatches(_ added: Set<String>, expect: String) -> Bool {
    let want = expect.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
    guard ocrVerifiable(expect) else { return false }
    let minFragment = max(4, (want.count + 1) / 2)
    return added.contains {
        $0.contains(want) || (want.contains($0) && $0.count >= minFragment)
    }
}

/// Whether a typed run is long enough for OCR delta to mean anything.
/// Below 4 characters (a digit into an OTP field) the on-screen
/// keyboard's own key labels contain the run, so nothing OCR finds
/// distinguishes typing from the keyboard appearing — and the two
/// accurate Vision passes per observation are pure cost. Short runs
/// skip OCR entirely and rely on the pixel-delta check alone.
func ocrVerifiable(_ expect: String) -> Bool {
    expect.trimmingCharacters(in: .whitespacesAndNewlines).count >= 4
}

/// A square region (capture pixels) around a touched point, big enough
/// to contain the visual response of a tap on a control.
func touchRegion(_ x: Double, _ y: Double, radius: Double = 96) -> CGRect {
    CGRect(x: x - radius, y: y - radius, width: radius * 2, height: radius * 2)
}

/// Perform an input action and answer with evidence that it landed.
///
/// The helper has no delivery signal to report: a background record post
/// returns "accepted", the focused CGEvent path returns nothing at all,
/// and iPhone Mirroring exposes no accessibility tree to re-read. So the
/// post-condition is pixels: capture the window before the gesture and
/// after it, and require the region it touched to have changed. A miss
/// re-observes with a longer settle (mirrored video can render a frame
/// late, and an animation can start slowly) — the gesture itself is never
/// re-posted, so nothing can be delivered twice — and running out of
/// observations is a typed "input-not-delivered" failure — never a
/// hopeful ok:true.
///
/// `region` is nil for whole-frame checks (typing, shortcuts, scroll).
///
/// `expectText` adds OCR text-delta verification for `type`: a few typed
/// glyphs in a ~1200x2600 frame move a pixel count near the video-noise
/// floor, so a pixel diff barely distinguishes real typing from nothing.
/// What the text recognizer reads is stronger evidence — but only under
/// tight rules, or unrelated screen activity gets credited to the
/// keystrokes: the recognized-text signatures are normalized sets with
/// the status bar excluded (OCR.textSignature), only *additions* to the
/// before-set count, and an addition only verifies when it matches the
/// text that was supposed to be typed. Any other change (a notification
/// banner, an animating label, recognition jitter) falls through to the
/// pixel-delta check as before.
///
/// With verification enabled, a window that cannot be captured is a
/// refusal, not a blind post: gestures that nobody can vouch for are
/// exactly what #204 set out to stop. `MIRROR_VERIFY=0` / `--verify off`
/// is the explicit opt-out, and then the response says `verify:"off"`,
/// `verified:false` rather than implying evidence that does not exist.
func runInput(region: CGRect?, expectText: String? = nil,
              _ perform: () throws -> Input.Outcome) throws -> [String: Any] {
    func payload(_ outcome: Input.Outcome, verify: String, verified: Bool,
                 delta: Double?) -> [String: Any] {
        var out: [String: Any] = [
            "ok": true, "verified": verified, "verify": verify, "mode": outcome.mode,
        ]
        if let d = delta { out["delta"] = (d * 100_000).rounded() / 100_000 }
        if let note = outcome.note { out["note"] = note }
        return out
    }
    guard Capability.verificationEnabled() else {
        return payload(try perform(), verify: "off", verified: false, delta: nil)
    }
    let first: CGImage
    do {
        first = try MirrorWindow.captureImage().0
    } catch {
        let why = (error as? MirrorWindow.HelperError)?.message ?? "\(error)"
        throw MirrorWindow.HelperError(
            code: "capture-failed",
            message: "input needs a capture to verify it was delivered, and capturing the window failed: \(why) (run --verify off / MIRROR_VERIFY=0 to post input without evidence)")
    }
    let grid = region == nil ? verifyWholeFrameGrid : 64
    let threshold = region == nil ? verifyWholeFrameChangedFraction : verifyChangedFraction
    let outcome = try perform()
    // The before-signature is recognized from the pre-action frame, but
    // the (slow) recognition itself runs only after the input is posted:
    // an accurate Vision pass between the frontmost check and the key
    // posts would widen the window in which focus can move elsewhere.
    var beforeTextMemo: Set<String>?
    var beforeTextRan = expectText == nil
    func beforeText() -> Set<String>? {
        if !beforeTextRan {
            beforeTextRan = true
            beforeTextMemo = OCR.textSignature(first)
        }
        return beforeTextMemo
    }
    var best = 0.0
    // A single blip (capture failed, or the frames disagree on size because
    // the window was resized or another backend served one of them) is not
    // the end of the check: keep observing, and only report the check as
    // unavailable if no observation ever completed. The "no change" verdict
    // stays reserved for a check that actually ran every time, so a missed
    // observation can never be read as evidence of nothing happening.
    var missedObservation = false
    for settle in verifySettleIntervals {
        Thread.sleep(forTimeInterval: settle)
        guard let after = try? MirrorWindow.captureImage().0 else {
            missedObservation = true
            continue
        }
        if let expect = expectText, let before = beforeText(),
           let afterText = OCR.textSignature(after),
           textAdditionMatches(afterText.subtracting(before), expect: expect) {
            return payload(outcome, verify: "ocr-delta", verified: true, delta: nil)
        }
        guard let delta = PixelDelta.changedFraction(first, after, region: region, grid: grid) else {
            missedObservation = true
            continue
        }
        best = max(best, delta)
        if delta >= threshold {
            return payload(outcome, verify: "pixel-delta", verified: true, delta: delta)
        }
    }
    if missedObservation {
        return payload(outcome, verify: "unavailable", verified: false, delta: nil)
    }
    throw MirrorWindow.HelperError(
        code: "input-not-delivered",
        message: "\(outcome.mode) input produced no change on the phone across \(verifySettleIntervals.count) observations (largest delta \(((best * 100_000).rounded() / 100_000)), threshold \(threshold)); the events were posted once but nothing observably changed")
}

func errorPayload(_ e: Error) -> (Int, [String: Any]) {
    if let he = e as? MirrorWindow.HelperError {
        let status: Int
        switch he.code {
        case "bad-request", "untypable": status = 400
        case "focus-lost": status = 503 // input gate: another app holds focus
        // post-condition failed: the events were posted, the phone did
        // not change. Retryable from the caller's point of view.
        case "input-not-delivered": status = 503
        default: status = 503
        }
        return (status, ["code": he.code, "error": he.message])
    }
    return (500, ["code": "internal", "error": "\(e)"])
}

/// Splits a request path into route and query parameters (values are
/// taken verbatim; the tiny local API never needs percent-decoding).
func splitQuery(_ path: String) -> (String, [String: String]) {
    guard let q = path.firstIndex(of: "?") else { return (path, [:]) }
    var params: [String: String] = [:]
    for pair in path[path.index(after: q)...].split(separator: "&") {
        let kv = pair.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false)
        params[String(kv[0])] = kv.count == 2 ? String(kv[1]) : ""
    }
    return (String(path[..<q]), params)
}

func handle(method: String, path rawPath: String, bodyData: Data) -> (Int, [String: Any]) {
    let body = (try? JSONSerialization.jsonObject(with: bodyData)) as? [String: Any] ?? [:]
    let (path, query) = splitQuery(rawPath)
    do {
        switch (method, path) {
        case ("GET", "/v1/status"):
            // light=1 skips the capture+OCR interstitial check: window /
            // process / state presence only. Liveness pollers (the
            // target registry, dashboards) use it so status probes never
            // stall gestures behind a full-frame OCR pass on this
            // strictly serial worker. A blocked session reports "ready"
            // in light mode; interstitials still surface on the OCR
            // paths (full status, observe, screenshot).
            let light = query["light"] == "1" || query["light"] == "true"
            let state: String
            let marker: String
            if light {
                if MirrorWindow.runningApp() == nil {
                    state = "not-running"
                } else if MirrorWindow.find() == nil {
                    state = "no-window"
                } else {
                    state = "ready"
                }
                marker = ""
            } else {
                (state, marker) = MirrorWindow.connectionState()
            }
            let mode = Capability.inputMode()
            var out: [String: Any] = [
                "state": state, "frontmost": MirrorWindow.isFrontmost(),
                // background == "input does not need the Mac's focus";
                // input_mode is what the next action will actually use.
                "background": mode == "background", "input_mode": mode,
                // Whether scroll can use the background path at all
                // (symbol probe). Delivery there is best-effort: a
                // verified miss falls back to the focused path per action.
                "background_scroll": mode != "focused" && BackgroundInput.supportsScroll,
                "capture": Capture.activeName(),
                "verify": Capability.verificationEnabled() ? "pixel-delta" : "off",
            ]
            if light { out["probe"] = "light" }
            if let w = MirrorWindow.find() { out["window"] = w.json }
            if !marker.isEmpty { out["marker"] = marker }
            if mode != "background", !Capability.backgroundBlocker().isEmpty {
                out["background_blocker"] = Capability.backgroundBlocker()
            }
            return (200, out)

        case ("GET", "/v1/doctor"):
            return (200, Doctor.report())

        case ("POST", "/v1/tap"):
            guard let x = num(body, "x"), let y = num(body, "y") else {
                return (400, ["code": "bad-request", "error": "tap requires numeric x and y"])
            }
            let target = try MirrorWindow.ensureInputReady(for: .mouse)
            try checkBounds(x, y, target.window)
            let (sx, sy) = toScreen(x, y, target.window)
            let requested = num(body, "duration_ms")
            let durationMs = clampMs(requested, default: 0)
            var out = try runInput(region: touchRegion(x, y)) {
                try Input.tap(x: sx, y: sy, durationMs: durationMs, target: target)
            }
            stampDuration(&out, requested: requested, applied: durationMs)
            return (200, out)

        case ("POST", "/v1/swipe"):
            guard let x1 = num(body, "x1"), let y1 = num(body, "y1"),
                  let x2 = num(body, "x2"), let y2 = num(body, "y2") else {
                return (400, ["code": "bad-request", "error": "swipe requires numeric x1, y1, x2, y2"])
            }
            let target = try MirrorWindow.ensureInputReady(for: .mouse)
            try checkBounds(x1, y1, target.window)
            try checkBounds(x2, y2, target.window)
            let (sx1, sy1) = toScreen(x1, y1, target.window)
            let (sx2, sy2) = toScreen(x2, y2, target.window)
            let requested = num(body, "duration_ms")
            let durationMs = clampMs(requested, default: 500)
            // A swipe changes the whole scroll view, not just its
            // endpoints: verify against the union of both touch regions.
            let region = touchRegion(x1, y1).union(touchRegion(x2, y2))
            var out = try runInput(region: region) {
                try Input.drag(x1: sx1, y1: sy1, x2: sx2, y2: sy2,
                               durationMs: durationMs, target: target)
            }
            stampDuration(&out, requested: requested, applied: durationMs)
            return (200, out)

        case ("POST", "/v1/scroll"):
            guard let x = num(body, "x"), let y = num(body, "y"), let dy = num(body, "dy") else {
                return (400, ["code": "bad-request", "error": "scroll requires numeric x, y, dy"])
            }
            // Background where the scroll-record symbol resolves, focused
            // otherwise (see MirrorWindow.InputKind.scroll).
            let target = try MirrorWindow.ensureInputReady(for: .scroll)
            try checkBounds(x, y, target.window)
            let (sx, sy) = toScreen(x, y, target.window)
            let dyPoints = dy / scaleFor(target.window)
            // The pre-gesture frame, held here so the fallback below can
            // rule out a late render before re-posting anything: the
            // no-change verdict can simply mean the phone rendered after
            // the observation budget, and a replay on top of a scroll
            // that did land would move the list twice as far.
            let pre = (target.isBackground && Capability.verificationEnabled())
                ? try? MirrorWindow.captureImage().0 : nil
            // What actually delivered, not what we asked for:
            // Input.scroll degrades to the focused path itself when the
            // WindowServer refuses the first record, and a focused burst
            // that verifies as no-change must not be posted twice.
            var deliveredMode: String?
            do {
                let out = try runInput(region: nil) {
                    let outcome = try Input.scroll(x: sx, y: sy, dy: dyPoints, target: target)
                    deliveredMode = outcome.mode
                    return outcome
                }
                return (200, out)
            } catch let e as MirrorWindow.HelperError
                where e.code == "input-not-delivered" && deliveredMode == "background" {
                // Background scroll delivery is intermittent: the
                // WindowServer accepts records that move nothing on the
                // phone while taps keep landing. A verified miss falls
                // back to the focused path rather than reporting a
                // hopeful failure the caller would just retry — unless
                // --background=on, which forbids taking the Mac's focus:
                // then the miss is a typed refusal naming what happened.
                // Last look before concluding anything: the verdict is
                // fallible (the phone can render after the observation
                // budget), and a replayed burst on a scroll that landed
                // late moves the list twice. One more capture against
                // the pre-gesture frame settles it — a late change means
                // the scroll delivered and the answer is a verified
                // success, not a replay or a refusal.
                guard let pre else {
                    throw MirrorWindow.HelperError(
                        code: "scroll-not-delivered",
                        message: "background scroll records were accepted but the late-render check could not capture the pre-gesture frame; no focused re-delivery was attempted")
                }
                Thread.sleep(forTimeInterval: verifySettleIntervals.last ?? 0.75)
                guard let late = try? MirrorWindow.captureImage().0 else {
                    throw MirrorWindow.HelperError(
                        code: "scroll-not-delivered",
                        message: "background scroll records were accepted but the late-render check could not capture the phone; no focused re-delivery was attempted")
                }
                guard let delta = PixelDelta.changedFraction(
                    pre, late, region: nil, grid: verifyWholeFrameGrid) else {
                    throw MirrorWindow.HelperError(
                        code: "scroll-not-delivered",
                        message: "background scroll records were accepted but the late-render check could not compare the phone frame; no focused re-delivery was attempted")
                }
                if delta >= verifyWholeFrameChangedFraction {
                    return (200, [
                        "ok": true, "verified": true, "verify": "pixel-delta",
                        "mode": "background",
                        "delta": (delta * 100_000).rounded() / 100_000,
                        "note": "background scroll rendered late: the change appeared after the verification budget, so nothing was re-posted",
                    ])
                }
                if MirrorConfig.shared.background == .on {
                    throw MirrorWindow.HelperError(
                        code: "scroll-not-delivered",
                        message: "background scroll records were accepted but nothing observably changed on the phone (delivery on this path is intermittent), and --background=on forbids the focused fallback; \(e.message)")
                }
                // The background records already went out, so from here
                // on "nothing was sent" codes would be a lie: a focus
                // loss while arranging the re-delivery re-surfaces as the
                // posted-but-unproven scroll-not-delivered (the daemon's
                // refusal-only swipe fallback must not put a drag on top
                // of records that may yet land).
                // The re-delivery activates the mirroring window, so
                // remember who held focus and hand it back afterwards —
                // an end-of-list scroll (a screen that genuinely cannot
                // move) reaches this path routinely and must not leave
                // the Mac's desktop taken over. The hand-back runs on
                // every exit, and the note reports what it actually did.
                let prevFront = MirrorWindow.frontmostApp()
                func giveFocusBack(after pid: pid_t) -> String {
                    guard let prev = prevFront, prev.pid != pid else { return "" }
                    return MirrorWindow.restoreFrontmost(pid: prev.pid)
                        ? "; \(prev.name) took focus back"
                        : "; \(prev.name) could not be given focus back"
                }
                do {
                    let focused = try MirrorWindow.focusedTarget()
                    var out: [String: Any]
                    do {
                        out = try runInput(region: nil) {
                            try Input.scroll(x: sx, y: sy, dy: dyPoints, target: focused)
                        }
                    } catch {
                        _ = giveFocusBack(after: focused.pid)
                        throw error
                    }
                    let note = "background scroll records were accepted but nothing changed; fell back to the focused path"
                        + giveFocusBack(after: focused.pid)
                    out["note"] = (out["note"] as? String).map { "\(note); \($0)" } ?? note
                    return (200, out)
                } catch let f as MirrorWindow.HelperError
                    // The background records already went out, so every
                    // way the re-delivery fails to *run* — focus refused,
                    // or its own pre-capture failing — must surface as
                    // posted-but-unproven, never as a code the daemon
                    // reads as "nothing was sent" and flicks on top of.
                    where f.code == "focus-lost" || f.code == "input-refused"
                        || f.code == "capture-failed" {
                    throw MirrorWindow.HelperError(
                        code: "scroll-not-delivered",
                        message: "background scroll records were accepted but nothing observably changed, and the focused re-delivery could not run: \(f.message)")
                }
            }

        case ("POST", "/v1/type"):
            guard let text = body["text"] as? String, !text.isEmpty else {
                return (400, ["code": "bad-request", "error": "type requires a non-empty text field"])
            }
            // Reject untypable text before resolving a target: resolving
            // .typeText can activate the mirroring window, and a doomed
            // request must not steal the Mac's focus.
            try Input.validateTypable(text: text)
            // Typing needs the window frontmost (see InputKind.typeText),
            // so remember who held focus to hand it back afterwards.
            let prevFront = MirrorWindow.frontmostApp()
            let target = try MirrorWindow.ensureInputReady(for: .typeText)
            func restoreNote() -> String? {
                guard target.mode == "focused-degrade" else { return nil }
                var note = "type ran on the focused path: iOS routes key events into text input only while iPhone Mirroring is frontmost, so the window was activated for the duration of the typing (the iOS on-screen keyboard never appears under mirroring; that is normal)"
                if let prev = prevFront, prev.pid != target.pid {
                    note += MirrorWindow.restoreFrontmost(pid: prev.pid)
                        ? "; \(prev.name) took focus back"
                        : "; \(prev.name) could not be given focus back"
                }
                return note
            }
            var out: [String: Any]
            do {
                out = try runInput(region: nil,
                                   expectText: ocrVerifiable(text) ? text : nil) {
                    try Input.type(text: text, target: target)
                }
            } catch {
                _ = restoreNote()
                throw error
            }
            if let note = restoreNote() {
                out["note"] = (out["note"] as? String).map { "\($0); \(note)" } ?? note
            }
            return (200, out)

        case ("POST", "/v1/press"):
            guard let combo = body["combo"] as? String, !combo.isEmpty else {
                return (400, ["code": "bad-request", "error": "press requires a combo field"])
            }
            guard allowedCombos.contains(combo) else {
                return (400, ["code": "bad-request",
                              "error": "combo \(combo) is not an iPhone Mirroring shortcut (allowed: \(allowedCombos.sorted().joined(separator: ", ")))"])
            }
            let target = try MirrorWindow.ensureInputReady(for: .keyboard)
            let out = try runInput(region: nil) {
                try Input.press(combo: combo, target: target)
            }
            return (200, out)

        case ("GET", "/v1/screenshot"):
            let (png, win) = try MirrorWindow.capture()
            let (w, h) = try OCR.imageSize(png: png)
            try checkCaptureSize(imgW: w, win: win)
            lastScale = MirrorWindow.scale(imgW: w, window: win)
            return (200, ["png_base64": png.base64EncodedString(), "img_w": w, "img_h": h,
                          "scale": lastScale])

        // POST accepts JSON options {level, rect, max_dim}; a bare GET
        // keeps the legacy behavior (accurate, full frame, no downscale).
        case ("GET", "/v1/ocr"), ("POST", "/v1/ocr"):
            let opts = method == "POST" ? try ocrOptions(body) : OCR.Options()
            let (png, win) = try MirrorWindow.capture()
            let (w, h) = try OCR.imageSize(png: png)
            try checkCaptureSize(imgW: w, win: win)
            lastScale = MirrorWindow.scale(imgW: w, window: win)
            let (boxes, servedScale) = try OCR.recognize(png: png, window: win, options: opts)
            let text = boxes.map { $0.text.lowercased() }.joined(separator: " ")
            for marker in MirrorWindow.blockedMarkers where text.contains(marker) {
                throw MirrorWindow.HelperError(code: "blocked",
                                               message: "iPhone Mirroring shows a connect/paused interstitial: \(marker)")
            }
            var out: [String: Any] = ["boxes": boxes.map { $0.json }, "img_w": w, "img_h": h,
                                      "scale": lastScale]
            if method == "POST" {
                out["served_scale"] = servedScale
                out["level"] = opts.levelName
                if let r = opts.rect {
                    out["rect"] = ["x": r.origin.x, "y": r.origin.y, "w": r.width, "h": r.height]
                }
            }
            return (200, out)

        default:
            return (404, ["code": "bad-request", "error": "no route \(method) \(path)"])
        }
    } catch {
        return errorPayload(error)
    }
}

// Startup capability probe: resolve input mode and capture backend once
// and record them in the LaunchAgent log, so a host that silently
// degraded to the focused path is diagnosable without a request.
Capability.logStartupProbe()

do {
    try HTTPServer(socketPath: socketPath, handler: handle).run()
} catch {
    FileHandle.standardError.write("mirrord: \(error.localizedDescription)\n".data(using: .utf8)!)
    exit(1)
}
