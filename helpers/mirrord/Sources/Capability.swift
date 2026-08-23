import AppKit
import CoreGraphics
import Foundation

/// What this host can actually do, established by probing rather than by
/// asking TCC.
///
/// `AXIsProcessTrusted()` and `CGPreflightScreenCaptureAccess()` are
/// answers about database rows, and they are wrong in both directions on
/// real Macs: preflight says false where capture works, and a granted
/// row says nothing about whether an event record reaches a window. So
/// the helper probes: post a harmless mouse-moved record and see that
/// the window survives, grab a frame and see that it is not blank.
enum Capability {
    private static let lock = NSLock()
    private static var probedKey: String?
    private static var probedOK = false
    private static var probedAt = Date.distantPast
    private static var blocker = "not probed"

    /// How long a *failed* probe is believed before re-probing. A success
    /// is cached for the life of the (window, pid); a failure can be a
    /// transient (the WindowServer refusing one record mid-reconnect),
    /// and believing it forever would pin every later action to the
    /// focus-stealing fallback for the whole mirroring session.
    private static let failureRetryInterval: TimeInterval = 30

    /// Is this a real on-console GUI session? Nothing works from a
    /// headless SSH session with no logged-in user: no WindowServer
    /// connection, no capture, no event delivery.
    static func onConsole() -> Bool {
        guard let dict = CGSessionCopyCurrentDictionary() as NSDictionary? else { return false }
        return (dict["kCGSSessionOnConsoleKey"] as? Bool) ?? false
    }

    /// Whether focus-free input can be used for this window, probed per
    /// (window, pid) and cached (a failure is re-probed after a short
    /// interval). A probe posts one mouse-moved
    /// record — the cheapest event that changes nothing on the phone —
    /// and requires the WindowServer to accept it and the window to
    /// still be there afterwards.
    static func backgroundInput(window: MirrorWindow.Window, pid: pid_t) -> Bool {
        switch MirrorConfig.shared.background {
        case .off:
            set(blocker: "disabled by --background=off/MIRROR_BACKGROUND=0")
            return false
        case .on, .auto:
            break
        }
        guard BackgroundInput.available else {
            set(blocker: "SLPSPostEventRecordTo/GetProcessForPID not resolvable on this macOS")
            return false
        }
        let key = "\(window.id):\(pid)"
        lock.lock()
        if probedKey == key,
           probedOK || Date().timeIntervalSince(probedAt) < failureRetryInterval {
            let cached = probedOK
            lock.unlock()
            return cached
        }
        lock.unlock()
        let ok = probe(window: window, pid: pid)
        lock.lock()
        probedKey = key
        probedOK = ok
        probedAt = Date()
        lock.unlock()
        return ok
    }

    /// Why background input is unavailable, for error messages and
    /// /v1/doctor.
    static func backgroundBlocker() -> String {
        lock.lock()
        defer { lock.unlock() }
        return blocker
    }

    /// Per-action verification is possible only when a capture backend
    /// can produce the before/after frames.
    ///
    /// A *misconfigured* pin (a backend this host cannot run) still counts
    /// as verification wanted: dropping to `verify:"off"` there would turn
    /// a typo into a silent loss of the delivery guarantee, indistinguish-
    /// able from the deliberate `--verify off`. Input then refuses with
    /// `capture-failed`, matching what /v1/screenshot already does.
    static func verificationEnabled() -> Bool {
        guard MirrorConfig.shared.verify != .off else { return false }
        return Capture.selected() != nil || Capture.pinnedUnavailable() != nil
    }

    private static func probe(window: MirrorWindow.Window, pid: pid_t) -> Bool {
        let center = CGPoint(x: window.x + window.w / 2, y: window.y + window.h / 2)
        let local = CGPoint(x: window.w / 2, y: window.h / 2)
        guard BackgroundInput.postMouse(.mouseMoved, pid: pid, wid: window.id,
                                        global: center, local: local) else {
            set(blocker: "the WindowServer refused a probe mouse record for window \(window.id)")
            return false
        }
        guard MirrorWindow.find() != nil else {
            set(blocker: "the mirroring window disappeared during the probe")
            return false
        }
        set(blocker: "")
        return true
    }

    private static func set(blocker why: String) {
        lock.lock()
        blocker = why
        lock.unlock()
    }

    /// Startup probe: resolve input mode and capture backend once, and
    /// say so on stderr (the LaunchAgent log) so a host that quietly
    /// degraded to the focused path is diagnosable without a request.
    static func logStartupProbe() {
        var mode = "focused"
        if let win = MirrorWindow.find(), let pid = MirrorWindow.runningApp()?.processIdentifier {
            mode = backgroundInput(window: win, pid: pid) ? "background" : "focused"
        } else if MirrorConfig.shared.background != .off, BackgroundInput.available {
            mode = "background?"  // symbols present; no window to probe yet
        }
        var line = "mirrord: input_mode=\(mode) capture=\(Capture.activeName())"
        line += " verify=\(verificationEnabled() ? "pixel-delta" : "off")"
        if mode != "background", !backgroundBlocker().isEmpty {
            line += " background_blocked=\"\(backgroundBlocker())\""
        }
        FileHandle.standardError.write(Data((line + "\n").utf8))
    }

    /// The input mode for /v1/status — cache-only, never probes. Status
    /// is polled by read-only paths (the daemon's target registry hits it
    /// on plain listings, including for devices whose input gate is off),
    /// and a probe posts a real mouse record into the phone window; that
    /// stays reserved for the actual input paths (`ensureInputReady`,
    /// `/v1/doctor`, the startup probe). Before any probe has run against
    /// the current window this reports "background?": symbols are
    /// present, delivery not yet demonstrated.
    static func inputMode() -> String {
        if case .off = MirrorConfig.shared.background { return "focused" }
        guard BackgroundInput.available,
              let win = MirrorWindow.find(),
              let pid = MirrorWindow.runningApp()?.processIdentifier else { return "focused" }
        let key = "\(win.id):\(pid)"
        lock.lock()
        defer { lock.unlock() }
        if probedKey == key { return probedOK ? "background" : "focused" }
        return "background?"
    }
}
