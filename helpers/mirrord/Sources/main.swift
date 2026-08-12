import AppKit
import CoreGraphics
import Foundation

/// mirrord — the manzanasd iPhone Mirroring helper.
///
/// A small GUI-session process that drives macOS iPhone Mirroring with
/// CGEvents (input), screencapture (eyes), and Vision OCR (the poor
/// man's element tree). manzanasd talks to it over a local unix socket;
/// mirrord is the process that holds the Accessibility + Screen
/// Recording TCC grants, which is why it must run as a LaunchAgent in
/// the logged-in user's GUI session — a daemon started over SSH is a
/// different TCC subject and gets nothing.
///
/// Protocol: HTTP/1.1 + JSON over the unix socket. Input coordinates
/// are capture-image pixels (the space /screenshot and /ocr report);
/// mirrord converts to Mac screen points internally.

let defaultSocket = NSString(string: "~/.manzanasd/mirrord.sock").expandingTildeInPath

func usage() -> Never {
    FileHandle.standardError.write("""
    usage: mirrord [--socket <path>] [--doctor]
      --socket  unix socket to serve on (default \(defaultSocket))
      --doctor  print permission/session checks and exit
    """.data(using: .utf8)!)
    exit(2)
}

var socketPath = defaultSocket
var doctor = false
var args = Array(CommandLine.arguments.dropFirst())
while !args.isEmpty {
    switch args.removeFirst() {
    case "--socket":
        guard !args.isEmpty else { usage() }
        socketPath = args.removeFirst()
    case "--doctor":
        doctor = true
    default:
        usage()
    }
}

/// Doctor output: every prerequisite in the input/capture chain, so the
/// user knows exactly which System Settings toggle is missing.
func runDoctor() -> Never {
    let ax = AXIsProcessTrusted()
    let screen = CGPreflightScreenCaptureAccess()
    let app = MirrorWindow.runningApp() != nil
    let window = MirrorWindow.find() != nil
    print("accessibility (taps/keys):    \(ax ? "granted" : "MISSING — System Settings > Privacy & Security > Accessibility > add mirrord")")
    print("screen recording (capture):   \(screen ? "granted" : "MISSING — System Settings > Privacy & Security > Screen & System Audio Recording > add mirrord")")
    print("iPhone Mirroring running:     \(app ? "yes" : "NO — open the iPhone Mirroring app")")
    print("phone window present:         \(window ? "yes" : "NO — connect the phone in iPhone Mirroring")")
    if ax && screen && app && window {
        let (state, marker) = MirrorWindow.connectionState()
        print("session state:                \(state)\(marker.isEmpty ? "" : " (\(marker))")")
        exit(state == "ready" ? 0 : 1)
    }
    if !screen {
        // Prompts the TCC dialog / registers mirrord in the pane.
        CGRequestScreenCaptureAccess()
    }
    if !ax {
        let opts = ["AXTrustedCheckOptionPrompt": true] as CFDictionary
        AXIsProcessTrustedWithOptions(opts)
    }
    exit(1)
}

if doctor { runDoctor() }

/// Capture scale (image px per window point) measured by the most
/// recent capture. Inputs prefer the live per-display factor below;
/// this cache is the fallback and also feeds /v1/scroll's pixel delta.
var lastScale: Double = Double(NSScreen.main?.backingScaleFactor ?? 2.0)

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

func errorPayload(_ e: Error) -> (Int, [String: Any]) {
    if let he = e as? MirrorWindow.HelperError {
        let status: Int
        switch he.code {
        case "bad-request", "untypable": status = 400
        default: status = 503
        }
        return (status, ["code": he.code, "error": he.message])
    }
    return (500, ["code": "internal", "error": "\(e)"])
}

func handle(method: String, path: String, bodyData: Data) -> (Int, [String: Any]) {
    let body = (try? JSONSerialization.jsonObject(with: bodyData)) as? [String: Any] ?? [:]
    do {
        switch (method, path) {
        case ("GET", "/v1/status"):
            let (state, marker) = MirrorWindow.connectionState()
            var out: [String: Any] = ["state": state, "frontmost": MirrorWindow.isFrontmost()]
            if let w = MirrorWindow.find() { out["window"] = w.json }
            if !marker.isEmpty { out["marker"] = marker }
            return (200, out)

        case ("POST", "/v1/tap"):
            guard let x = num(body, "x"), let y = num(body, "y") else {
                return (400, ["code": "bad-request", "error": "tap requires numeric x and y"])
            }
            let win = try MirrorWindow.ensureInputReady()
            try checkBounds(x, y, win)
            let (sx, sy) = toScreen(x, y, win)
            Input.tap(x: sx, y: sy, durationMs: clampMs(num(body, "duration_ms"), default: 0))
            return (200, ["ok": true])

        case ("POST", "/v1/swipe"):
            guard let x1 = num(body, "x1"), let y1 = num(body, "y1"),
                  let x2 = num(body, "x2"), let y2 = num(body, "y2") else {
                return (400, ["code": "bad-request", "error": "swipe requires numeric x1, y1, x2, y2"])
            }
            let win = try MirrorWindow.ensureInputReady()
            try checkBounds(x1, y1, win)
            try checkBounds(x2, y2, win)
            let (sx1, sy1) = toScreen(x1, y1, win)
            let (sx2, sy2) = toScreen(x2, y2, win)
            Input.drag(x1: sx1, y1: sy1, x2: sx2, y2: sy2,
                       durationMs: clampMs(num(body, "duration_ms"), default: 500))
            return (200, ["ok": true])

        case ("POST", "/v1/scroll"):
            guard let x = num(body, "x"), let y = num(body, "y"), let dy = num(body, "dy") else {
                return (400, ["code": "bad-request", "error": "scroll requires numeric x, y, dy"])
            }
            let win = try MirrorWindow.ensureInputReady()
            try checkBounds(x, y, win)
            let (sx, sy) = toScreen(x, y, win)
            Input.scroll(x: sx, y: sy, dy: dy / scaleFor(win))
            return (200, ["ok": true])

        case ("POST", "/v1/type"):
            guard let text = body["text"] as? String, !text.isEmpty else {
                return (400, ["code": "bad-request", "error": "type requires a non-empty text field"])
            }
            _ = try MirrorWindow.ensureInputReady()
            try Input.type(text: text)
            return (200, ["ok": true])

        case ("POST", "/v1/press"):
            guard let combo = body["combo"] as? String, !combo.isEmpty else {
                return (400, ["code": "bad-request", "error": "press requires a combo field"])
            }
            guard allowedCombos.contains(combo) else {
                return (400, ["code": "bad-request",
                              "error": "combo \(combo) is not an iPhone Mirroring shortcut (allowed: \(allowedCombos.sorted().joined(separator: ", ")))"])
            }
            _ = try MirrorWindow.ensureInputReady()
            try Input.press(combo: combo)
            return (200, ["ok": true])

        case ("GET", "/v1/screenshot"):
            let (png, win) = try MirrorWindow.capture()
            let (w, h) = try OCR.imageSize(png: png)
            lastScale = MirrorWindow.scale(imgW: w, window: win)
            return (200, ["png_base64": png.base64EncodedString(), "img_w": w, "img_h": h])

        case ("GET", "/v1/ocr"):
            let (png, win) = try MirrorWindow.capture()
            let (w, h) = try OCR.imageSize(png: png)
            lastScale = MirrorWindow.scale(imgW: w, window: win)
            let boxes = try OCR.recognize(png: png, window: win)
            let text = boxes.map { $0.text.lowercased() }.joined(separator: " ")
            for marker in MirrorWindow.blockedMarkers where text.contains(marker) {
                throw MirrorWindow.HelperError(code: "blocked",
                                               message: "iPhone Mirroring shows a connect/paused interstitial: \(marker)")
            }
            return (200, ["boxes": boxes.map { $0.json }, "img_w": w, "img_h": h])

        default:
            return (404, ["code": "bad-request", "error": "no route \(method) \(path)"])
        }
    } catch {
        return errorPayload(error)
    }
}

do {
    try HTTPServer(socketPath: socketPath, handler: handle).run()
} catch {
    FileHandle.standardError.write("mirrord: \(error.localizedDescription)\n".data(using: .utf8)!)
    exit(1)
}
