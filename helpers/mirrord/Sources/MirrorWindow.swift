import AppKit
import ApplicationServices
import CoreGraphics
import Foundation

/// iPhone Mirroring window discovery, input targeting, and capture.
///
/// The mirroring window is a video stream: macOS accessibility sees
/// nothing inside it, so input is synthesized and observation is capture
/// + OCR. Input reaches it in one of two modes (see Input): focus-free
/// CGS event records addressed to the mirroring process, or — as a
/// fallback — activate-then-post at the HID tap, which requires the
/// window to be frontmost. All bounds are Mac screen points; captures
/// are pixels (Retina scale applies).
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

        var json: [String: Any] { ["window_id": Int(id), "x": x, "y": y, "w": w, "h": h] }
    }

    struct HelperError: Error {
        let code: String
        let message: String
    }

    /// Everything one action needs to reach the phone: the window, the
    /// mirroring process to address records/keys to, and which input
    /// mode was resolved for it.
    struct InputTarget {
        let window: Window
        let pid: pid_t
        /// "background" (focus-free) or "focused" (activated).
        let mode: String

        var isBackground: Bool { mode == "background" }
    }

    /// Which input path an action needs. Scroll is separate because its
    /// background path additionally needs the scroll-record symbol
    /// (BackgroundInput.supportsScroll); where that symbol is missing,
    /// scroll can only be delivered through the focused path. Text
    /// typing is separate too: background key events reach the mirroring
    /// process (shortcuts like Cmd+1 work) but iOS routes keys into text
    /// input only while the window is frontmost, so on a
    /// background-capable host typing degrades to the focused path per
    /// action.
    enum InputKind {
        case mouse, keyboard, typeText, scroll
    }

    static func runningApp() -> NSRunningApplication? {
        NSRunningApplication.runningApplications(withBundleIdentifier: bundleID).first
    }

    /// The app that owns the Mac's focus right now, read live.
    ///
    /// NSWorkspace.frontmostApplication is refreshed by run-loop
    /// notifications that this helper (a socket-serving process with no
    /// AppKit event loop) never pumps, so it reports whichever app was
    /// frontmost when the process launched — forever. Gating focused
    /// input on that frozen answer either refuses input while the
    /// mirroring window genuinely holds focus, or posts HID events into
    /// whatever app the user switched to (#173). The accessibility
    /// system-wide element and the window list are queried on demand and
    /// answer for the current moment.
    static func frontmostApp() -> (pid: pid_t, name: String)? {
        var ref: CFTypeRef?
        if AXUIElementCopyAttributeValue(
            AXUIElementCreateSystemWide(), "AXFocusedApplication" as CFString, &ref) == .success,
            let v = ref, CFGetTypeID(v) == AXUIElementGetTypeID() {
            var pid: pid_t = 0
            if AXUIElementGetPid(v as! AXUIElement, &pid) == .success, pid > 0 {
                return (pid, appName(pid))
            }
        }
        // Without the Accessibility grant the AX read fails; the topmost
        // layer-0 on-screen window's owner is the next best live answer
        // (the list is z-ordered, so the first ordinary window wins).
        guard let wins = CGWindowListCopyWindowInfo(
            [.optionOnScreenOnly], kCGNullWindowID) as? [[String: Any]] else { return nil }
        for w in wins where (w["kCGWindowLayer"] as? Int) == 0 {
            guard let pid = w["kCGWindowOwnerPID"] as? Int32 else { continue }
            let name = (w["kCGWindowOwnerName"] as? String) ?? appName(pid)
            return (pid, name)
        }
        return nil
    }

    private static func appName(_ pid: pid_t) -> String {
        NSRunningApplication(processIdentifier: pid)?.localizedName ?? "pid \(pid)"
    }

    static func isFrontmost() -> Bool {
        guard let pid = runningApp()?.processIdentifier,
              let front = frontmostApp() else { return false }
        return front.pid == pid
    }

    /// The mirroring window's bounds, or nil. Panels/toolbars (width
    /// < 100) are ignored.
    ///
    /// The phone window is the largest layer-0 window of the pid;
    /// picking the frontmost wide-enough one lets a small overlay/HUD
    /// win and desynchronizes capture from input. When the app's AX
    /// standard windows are readable, the choice is narrowed to
    /// candidates matching one of them (largest AX-confirmed wins).
    static func find() -> Window? {
        // Match by owner PID via the bundle id: kCGWindowOwnerName is the
        // localized display name and differs on non-English systems.
        guard let pid = runningApp()?.processIdentifier,
              let wins = CGWindowListCopyWindowInfo(
                  [.optionOnScreenOnly], kCGNullWindowID) as? [[String: Any]] else { return nil }
        var candidates: [Window] = []
        for w in wins {
            guard (w["kCGWindowOwnerPID"] as? Int32) == pid,
                  (w["kCGWindowLayer"] as? Int ?? 1) == 0,
                  let b = w["kCGWindowBounds"] as? [String: Any],
                  let width = b["Width"] as? Double, width >= 100,
                  let x = b["X"] as? Double, let y = b["Y"] as? Double,
                  let h = b["Height"] as? Double,
                  let num = w["kCGWindowNumber"] as? Int
            else { continue }
            candidates.append(Window(id: CGWindowID(num), x: x, y: y, w: width, h: h))
        }
        guard let largest = candidates.max(by: { $0.w * $0.h < $1.w * $1.h }) else { return nil }
        // Area is the primary criterion (the phone window is always the
        // big one); AX confirmation only overrides it when the confirmed
        // window is comparably large. AX rects can lag the real window
        // (live resize/move), so a stale-AX phone window must not lose
        // to a small but AX-fresh settings/dialog window — and preferring
        // the frontmost AX window outright would re-introduce
        // frontmost-wins.
        let axRects = axStandardWindowRects(pid: pid)
        let confirmed = candidates.filter { c in axRects.contains { matchesAX(c, $0) } }
        if let best = confirmed.max(by: { $0.w * $0.h < $1.w * $1.h }),
           best.w * best.h >= largest.w * largest.h * 0.5 {
            return best
        }
        return largest
    }

    /// Bounds of the app's AX standard windows (screen points, top-left
    /// origin — same space as CGWindow bounds). Empty when AX is
    /// unreadable (the mirroring video surface exposes no children, but
    /// the window elements themselves are usually present).
    private static func axStandardWindowRects(pid: pid_t) -> [CGRect] {
        let app = AXUIElementCreateApplication(pid)
        var winsRef: CFTypeRef?
        guard AXUIElementCopyAttributeValue(app, kAXWindowsAttribute as CFString, &winsRef) == .success,
              let axWins = winsRef as? [AXUIElement] else { return [] }
        var rects: [CGRect] = []
        for axWin in axWins {
            var subroleRef: CFTypeRef?
            guard AXUIElementCopyAttributeValue(axWin, kAXSubroleAttribute as CFString, &subroleRef) == .success,
                  (subroleRef as? String) == kAXStandardWindowSubrole as String,
                  let pos = axPoint(axWin, kAXPositionAttribute),
                  let size = axSize(axWin, kAXSizeAttribute)
            else { continue }
            rects.append(CGRect(origin: pos, size: size))
        }
        return rects
    }

    private static func axPoint(_ el: AXUIElement, _ attr: String) -> CGPoint? {
        var ref: CFTypeRef?
        guard AXUIElementCopyAttributeValue(el, attr as CFString, &ref) == .success,
              let v = ref, CFGetTypeID(v) == AXValueGetTypeID() else { return nil }
        var p = CGPoint.zero
        guard AXValueGetValue(v as! AXValue, .cgPoint, &p) else { return nil }
        return p
    }

    private static func axSize(_ el: AXUIElement, _ attr: String) -> CGSize? {
        var ref: CFTypeRef?
        guard AXUIElementCopyAttributeValue(el, attr as CFString, &ref) == .success,
              let v = ref, CFGetTypeID(v) == AXValueGetTypeID() else { return nil }
        var s = CGSize.zero
        guard AXValueGetValue(v as! AXValue, .cgSize, &s) else { return nil }
        return s
    }

    /// AX and CGWindow report the same window with small offsets (title
    /// bar accounting, live-resize lag), so compare within a tolerance.
    private static let axTolerance = 8.0

    private static func matchesAX(_ w: Window, _ ax: CGRect) -> Bool {
        abs(w.x - ax.origin.x) <= axTolerance
            && abs(w.y - ax.origin.y) <= axTolerance
            && abs(w.w - ax.width) <= axTolerance
            && abs(w.h - ax.height) <= axTolerance
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

    /// Resolve the window for a capture — polling, never activating.
    /// Looking at the phone must not disturb whoever is using the Mac, so
    /// a window that is momentarily unlisted (reconnect, re-composite) is
    /// waited for and then reported as `no-window`; activation belongs to
    /// the focused input fallback alone.
    static func windowForCapture() throws -> Window {
        guard runningApp() != nil else {
            throw HelperError(code: "not-running",
                              message: "iPhone Mirroring is not running; open it and connect the phone")
        }
        for attempt in 0..<10 {
            if let w = find() { return w }
            if attempt < 9 { Thread.sleep(forTimeInterval: 0.2) }
        }
        throw HelperError(code: "no-window",
                          message: "iPhone Mirroring has no phone window; connect the phone in the app")
    }

    /// Capture the mirroring window in process (see Capture): one frame,
    /// no activation, no subprocess unless every in-process path failed.
    /// Retries once — a window that moved or re-composited between the
    /// lookup and the capture is the common transient.
    static func captureImage() throws -> (CGImage, Window, String) {
        var win = try windowForCapture()
        var lastError: Error?
        for attempt in 0..<2 {
            if attempt > 0 {
                Thread.sleep(forTimeInterval: 0.3)
                win = try windowForCapture()
            }
            do {
                let (image, backend) = try Capture.frame(window: win)
                return (image, win, backend)
            } catch {
                lastError = error
            }
        }
        throw lastError ?? HelperError(code: "capture-failed", message: "window capture failed")
    }

    /// Capture the mirroring window as PNG (the wire format for
    /// /v1/screenshot and the OCR input).
    static func capture() throws -> (Data, Window) {
        let (image, win, _) = try captureImage()
        return (try Capture.png(image), win)
    }

    /// "ready" | "blocked" | "no-window" | "not-running", plus the
    /// interstitial marker when blocked.
    static func connectionState() -> (String, String) {
        if runningApp() == nil { return ("not-running", "") }
        guard find() != nil else { return ("no-window", "") }
        guard let (png, win) = try? capture(),
              let (boxes, _) = try? OCR.recognize(png: png, window: win) else {
            return ("ready", "") // capture/OCR failure is reported by the op itself
        }
        let text = boxes.map { $0.text.lowercased() }.joined(separator: " ")
        for marker in blockedMarkers where text.contains(marker) {
            return ("blocked", marker)
        }
        return ("ready", "")
    }

    /// Guard for input ops: resolve the window and the mode its events
    /// will use. Cheap (no OCR) — a blocked interstitial still swallows
    /// input harmlessly and shows up on the next observe/screenshot.
    ///
    /// When focus-free input is available nothing is activated: the
    /// mirroring window does not have to be frontmost (that was the
    /// "cannot bring iPhone Mirroring frontmost" dead end) and the Mac's
    /// owner keeps their desktop. On the focused fallback the historical
    /// contract holds: activate, then verify frontmost rather than
    /// assume it, because events posted while another app holds focus
    /// would be swallowed or — worse — land in that app.
    static func ensureInputReady(for kind: InputKind = .mouse) throws -> InputTarget {
        // Resolved without activation: the mode decision (and the
        // --background=on refusal) must not itself grab the Mac's focus.
        // Only focusedTarget() activates, once the fallback is chosen.
        let win = try windowForCapture()
        guard let pid = runningApp()?.processIdentifier else {
            throw HelperError(code: "not-running",
                              message: "iPhone Mirroring is not running; open it and connect the phone")
        }
        if kind != .typeText, kind != .scroll || BackgroundInput.supportsScroll,
           Capability.backgroundInput(window: win, pid: pid) {
            return InputTarget(window: win, pid: pid, mode: "background")
        }
        if kind == .typeText, Capability.backgroundInput(window: win, pid: pid) {
            // Typing on a background-capable host degrades to the focused
            // path: `postToPid` key events reach the mirroring process,
            // but iOS only routes them into text input while the window
            // is frontmost, so backgrounded keystrokes land nowhere. The
            // degraded mode is named "focused-degrade" so callers can
            // tell a deliberate per-action degrade from a host stuck on
            // the focused fallback.
            if MirrorConfig.shared.background == .on {
                throw HelperError(
                    code: "input-refused",
                    message: "type needs the focused path (iOS routes key events into text input only while iPhone Mirroring is frontmost) and --background=on forbids it; no input was sent")
            }
            let t = try focusedTarget(window: win, pid: pid)
            return InputTarget(window: t.window, pid: t.pid, mode: "focused-degrade")
        }
        if MirrorConfig.shared.background == .on {
            throw HelperError(
                code: "input-refused",
                message: kind == .scroll && BackgroundInput.available && !BackgroundInput.supportsScroll
                    ? "scroll needs the focused path (SLEventRecordPointer is not resolvable on this macOS) and --background=on forbids it; no input was sent"
                    : "background input is unavailable on this host (\(Capability.backgroundBlocker())) and --background=on forbids the focused fallback; no input was sent")
        }
        return try focusedTarget(window: win, pid: pid)
    }

    /// The focused-path target: activate the window and confirm it is
    /// frontmost before any event is posted.
    static func focusedTarget(window: Window? = nil, pid: pid_t? = nil) throws -> InputTarget {
        let win = try window ?? ensureWindow()
        guard let pid = pid ?? runningApp()?.processIdentifier else {
            throw HelperError(code: "not-running",
                              message: "iPhone Mirroring is not running; open it and connect the phone")
        }
        try activate()
        for _ in 0..<10 {
            if isFrontmost() { return InputTarget(window: win, pid: pid, mode: "focused") }
            Thread.sleep(forTimeInterval: 0.1)
        }
        let holder = frontmostApp()?.name ?? "another app"
        throw HelperError(
            code: "focus-lost",
            message: "iPhone Mirroring could not be brought frontmost (\(holder) holds focus); input was not sent")
    }

    /// Hand focus back to the app that held it before a focused-path
    /// action activated the mirroring window. Best-effort: activating
    /// another process is a request the WindowServer may decline, so
    /// the return value reports whether the app actually became
    /// frontmost again.
    static func restoreFrontmost(pid: pid_t) -> Bool {
        // Do not steal focus back from a human who selected another app while
        // the mirror action was being verified.
        guard isFrontmost() else { return false }
        guard let app = NSRunningApplication(processIdentifier: pid),
              !app.isTerminated else { return false }
        guard app.activate(options: [.activateIgnoringOtherApps]) else { return false }
        for _ in 0..<10 {
            if frontmostApp()?.pid == pid { return true }
            Thread.sleep(forTimeInterval: 0.1)
        }
        return false
    }

    /// Pixel scale of a capture relative to window points (Retina).
    static func scale(imgW: Int, window: Window) -> Double {
        guard imgW > 0, window.w > 0 else { return 2.0 }
        return Double(imgW) / window.w
    }
}
