import AppKit
import CoreGraphics
import Foundation

/// iPhone Mirroring window discovery, focus, and capture.
///
/// The mirroring window is a video stream: macOS accessibility sees
/// nothing inside it, input must be synthesized at the HID tap, and the
/// window must be frontmost or events are swallowed. All bounds are Mac
/// screen points; captures are pixels (Retina scale applies).
enum MirrorWindow {
    static let bundleID = "com.apple.ScreenContinuity"

    /// Distinctive text on the not-connected interstitials. Only phrases
    /// specific enough to never appear in ordinary app content qualify:
    /// the whole OCR text is substring-matched, and a false positive
    /// makes a healthy screen unreadable. Getting past any of these is a
    /// physical action only the user can do (if it says "iPhone in Use",
    /// lock the phone).
    static let blockedMarkers = [
        "iphone in use", "lock your iphone to use iphone mirroring",
        "iphone mirroring has ended",
    ]

    struct Window {
        let id: CGWindowID
        let x: Double
        let y: Double
        let w: Double
        let h: Double

        var json: [String: Any] { ["x": x, "y": y, "w": w, "h": h] }
    }

    struct HelperError: Error {
        let code: String
        let message: String
    }

    static func runningApp() -> NSRunningApplication? {
        NSRunningApplication.runningApplications(withBundleIdentifier: bundleID).first
    }

    static func isFrontmost() -> Bool {
        NSWorkspace.shared.frontmostApplication?.bundleIdentifier == bundleID
    }

    /// The mirroring window's bounds, or nil. Panels/toolbars (width
    /// < 100) are ignored.
    static func find() -> Window? {
        // Match by owner PID via the bundle id: kCGWindowOwnerName is the
        // localized display name and differs on non-English systems.
        guard let pid = runningApp()?.processIdentifier,
              let wins = CGWindowListCopyWindowInfo(
                  [.optionOnScreenOnly], kCGNullWindowID) as? [[String: Any]] else { return nil }
        for w in wins {
            guard (w["kCGWindowOwnerPID"] as? Int32) == pid,
                  (w["kCGWindowLayer"] as? Int ?? 1) == 0,
                  let b = w["kCGWindowBounds"] as? [String: Any],
                  let width = b["Width"] as? Double, width >= 100,
                  let x = b["X"] as? Double, let y = b["Y"] as? Double,
                  let h = b["Height"] as? Double,
                  let num = w["kCGWindowNumber"] as? Int
            else { continue }
            return Window(id: CGWindowID(num), x: x, y: y, w: width, h: h)
        }
        return nil
    }

    /// Bring iPhone Mirroring frontmost. Never launches it: opening the
    /// app and connecting the phone is the user's job.
    static func activate() throws {
        guard let app = runningApp() else {
            throw HelperError(code: "not-running",
                              message: "iPhone Mirroring is not running; open it and connect the phone")
        }
        if !isFrontmost() {
            app.activate(options: [.activateIgnoringOtherApps])
            Thread.sleep(forTimeInterval: 0.5)
        }
    }

    static func ensureWindow() throws -> Window {
        if let w = find() { return w }
        try activate()
        for _ in 0..<10 {
            if let w = find() { return w }
            Thread.sleep(forTimeInterval: 0.5)
        }
        throw HelperError(code: "no-window",
                          message: "iPhone Mirroring has no phone window; connect the phone in the app")
    }

    /// Capture the mirroring window as PNG. `screencapture -l` grabs just
    /// the window but fails when it is not composited; the fallback
    /// activates it and region-captures its rect.
    static func capture() throws -> (Data, Window) {
        var win = try ensureWindow()
        let path = NSTemporaryDirectory() + "mirrord-capture.png"
        var lastErr = "empty capture"
        for attempt in 0..<3 {
            if attempt > 0 {
                try activate()
                win = try ensureWindow()
            }
            let windowArgs = ["-x", "-o", "-l", String(win.id), path]
            let region = "\(Int(win.x)),\(Int(win.y)),\(Int(win.w)),\(Int(win.h))"
            let regionArgs = ["-x", "-R", region, path]
            for args in [windowArgs, regionArgs] {
                let (ok, err) = runScreencapture(args, path: path)
                if ok {
                    let data = try Data(contentsOf: URL(fileURLWithPath: path))
                    return (data, win)
                }
                lastErr = err
                try activate()
            }
            Thread.sleep(forTimeInterval: 0.3)
        }
        throw HelperError(code: "capture-failed",
                          message: "window capture failed (is Screen Recording granted to mirrord?): \(lastErr)")
    }

    private static func runScreencapture(_ args: [String], path: String) -> (Bool, String) {
        try? FileManager.default.removeItem(atPath: path)
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/sbin/screencapture")
        p.arguments = args
        let errPipe = Pipe()
        p.standardError = errPipe
        do { try p.run() } catch { return (false, "\(error)") }
        p.waitUntilExit()
        let errData = errPipe.fileHandleForReading.readDataToEndOfFile()
        let stderr = String(data: errData, encoding: .utf8) ?? ""
        let size = (try? FileManager.default.attributesOfItem(atPath: path))?[.size] as? Int ?? 0
        let ok = p.terminationStatus == 0 && size > 1000
        return (ok, stderr.isEmpty ? "empty capture" : stderr.trimmingCharacters(in: .whitespacesAndNewlines))
    }

    /// "ready" | "blocked" | "no-window" | "not-running", plus the
    /// interstitial marker when blocked.
    static func connectionState() -> (String, String) {
        if runningApp() == nil { return ("not-running", "") }
        guard find() != nil else { return ("no-window", "") }
        guard let (png, win) = try? capture(),
              let boxes = try? OCR.recognize(png: png, window: win) else {
            return ("ready", "") // capture/OCR failure is reported by the op itself
        }
        let text = boxes.map { $0.text.lowercased() }.joined(separator: " ")
        for marker in blockedMarkers where text.contains(marker) {
            return ("blocked", marker)
        }
        return ("ready", "")
    }

    /// Guard for input ops: window present + frontmost. Cheap (no OCR) —
    /// a blocked interstitial still swallows input harmlessly and shows
    /// up on the next observe/screenshot.
    static func ensureInputReady() throws -> Window {
        let win = try ensureWindow()
        try activate()
        return win
    }

    /// Pixel scale of a capture relative to window points (Retina).
    static func scale(imgW: Int, window: Window) -> Double {
        guard imgW > 0, window.w > 0 else { return 2.0 }
        return Double(imgW) / window.w
    }
}
