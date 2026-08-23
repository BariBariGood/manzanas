import AppKit
import CoreGraphics
import Foundation

/// `mirrord --doctor` / `GET /v1/doctor`: the same capability report in
/// text and JSON.
///
/// These are *probes*, not TCC lookups. A granted Screen Recording row
/// does not mean a capture returns pixels (and `CGPreflightScreenCaptureAccess`
/// answers false on hosts where capture works fine), and no TCC row at
/// all describes whether an event record reaches a background window. So
/// the doctor grabs a real frame, checks it is not blank, posts a
/// harmless probe record, and reports what happened.
enum Doctor {
    struct Check {
        let name: String
        let ok: Bool
        let detail: String

        var json: [String: Any] { ["name": name, "ok": ok, "detail": detail] }
    }

    static func report() -> [String: Any] {
        var checks: [Check] = []

        let console = Capability.onConsole()
        checks.append(Check(
            name: "gui_session", ok: console,
            detail: console
                ? "on-console GUI session"
                : "no on-console GUI session: mirrord must run as a LaunchAgent in the logged-in user's session (a process started over SSH is a different TCC subject and cannot see the WindowServer)"))

        let app = MirrorWindow.runningApp()
        checks.append(Check(
            name: "mirroring_app", ok: app != nil,
            detail: app.map { "iPhone Mirroring running (pid \($0.processIdentifier))" }
                ?? "iPhone Mirroring is not running; open the app"))

        let window = MirrorWindow.find()
        checks.append(Check(
            name: "phone_window", ok: window != nil,
            detail: window.map {
                "window \($0.id) at (\(Int($0.x)), \(Int($0.y))) \(Int($0.w))x\(Int($0.h))pt"
            } ?? "no phone window; connect the phone in iPhone Mirroring"))

        checks.append(captureCheck(window: window))
        checks.append(backgroundCheck(window: window, pid: app?.processIdentifier))
        checks.append(backgroundScrollCheck(window: window, pid: app?.processIdentifier))

        let verify = Capability.verificationEnabled()
        checks.append(Check(
            name: "verification", ok: verify,
            detail: verify
                ? "per-action capture delta (responses carry verified/delta)"
                : "off: actions answer verified:false (--verify=off, or no capture backend)"))

        // Only the focused fallback needs Accessibility: background mouse
        // records and CGEvent.postToPid do not go through the HID tap.
        let ax = AXIsProcessTrusted()
        checks.append(Check(
            name: "accessibility", ok: ax,
            detail: ax
                ? "granted (needed by the focused fallback)"
                : "MISSING — System Settings > Privacy & Security > Accessibility > add mirrord; required for the focused fallback"))

        var state = "unknown"
        if window != nil {
            let (s, marker) = MirrorWindow.connectionState()
            state = marker.isEmpty ? s : "\(s) (\(marker))"
            checks.append(Check(name: "session_state", ok: s == "ready", detail: state))
        }

        let mode = Capability.inputMode()
        let required = ["gui_session", "mirroring_app", "phone_window", "capture", "session_state"]
        let hardFail = checks.contains { required.contains($0.name) && !$0.ok }
        let canInput = mode == "background" || ax
        return [
            "ok": !hardFail && canInput,
            "input_mode": mode,
            "background": mode == "background",
            "background_scroll": mode != "focused" && BackgroundInput.supportsScroll,
            "capture": Capture.activeName(),
            "verify": verify ? "pixel-delta" : "off",
            "state": state,
            "checks": checks.map { $0.json },
        ]
    }

    /// Capture probe: grab a frame and require it to be non-blank. "I got
    /// a black rectangle" is the characteristic shape of a capture that
    /// is technically permitted but useless.
    private static func captureCheck(window: MirrorWindow.Window?) -> Check {
        guard let win = window else {
            return Check(name: "capture", ok: false,
                         detail: "not probed: no phone window to capture")
        }
        // The binary the TCC grant must go to: name it in the detail so
        // the daemon's remedy line can point somewhere concrete.
        let selfPath = Bundle.main.executablePath ?? CommandLine.arguments.first ?? "mirrord"
        do {
            let (image, backend) = try Capture.frame(window: win)
            let spread = PixelDelta.meanAbsoluteDeviation(image) ?? 0
            let size = "\(image.width)x\(image.height)px via \(backend)"
            if spread <= 0.5 {
                return Check(name: "capture", ok: false,
                             detail: "\(size) but the frame is uniform (mean abs deviation \(rounded(spread))): capture is returning a blank window")
            }
            return Check(name: "capture", ok: true,
                         detail: "\(size), mean abs deviation \(rounded(spread))")
        } catch let e as MirrorWindow.HelperError {
            return Check(name: "capture", ok: false,
                         detail: "\(e.message) (responsible process: \(selfPath))")
        } catch {
            return Check(name: "capture", ok: false,
                         detail: "\(error) (responsible process: \(selfPath))")
        }
    }

    /// Background-input probe: symbols resolvable *and* the WindowServer
    /// accepting a no-op record for this window.
    private static func backgroundCheck(window: MirrorWindow.Window?, pid: pid_t?) -> Check {
        guard MirrorConfig.shared.background != .off else {
            // A deliberate configuration choice, not a broken capability:
            // reporting it as a failure sent operators chasing TCC grants
            // they already had.
            return Check(name: "background_input", ok: true,
                         detail: "disabled by --background=off/MIRROR_BACKGROUND=0; input uses the focused path and will take over the Mac's focus")
        }
        guard BackgroundInput.available else {
            return Check(name: "background_input", ok: false,
                         detail: "SLPSPostEventRecordTo/GetProcessForPID not resolvable on this macOS; input falls back to the focused path")
        }
        guard let win = window, let pid = pid else {
            return Check(name: "background_input", ok: false,
                         detail: "not probed: no phone window/process to post a probe record to (symbols are present)")
        }
        let ok = Capability.backgroundInput(window: win, pid: pid)
        return Check(name: "background_input", ok: ok,
                     detail: ok
                         ? "probe record accepted for window \(win.id) (pid \(pid)); input needs no focus"
                         : "probe failed: \(Capability.backgroundBlocker())")
    }

    /// Scroll-record probe: whether the extra symbol the background
    /// scroll path needs resolves. Symbol presence is not delivery —
    /// accepted scroll records intermittently move nothing on real
    /// hosts — so scroll stays verify-backed per action, with the
    /// focused path as the fallback; this check only says which path
    /// the next scroll will try first.
    private static func backgroundScrollCheck(window: MirrorWindow.Window?, pid: pid_t?) -> Check {
        guard MirrorConfig.shared.background != .off else {
            return Check(name: "background_scroll", ok: true,
                         detail: "disabled by --background=off/MIRROR_BACKGROUND=0; scroll uses the focused path")
        }
        guard BackgroundInput.available else {
            return Check(name: "background_scroll", ok: false,
                         detail: "background input symbols are unavailable on this host; scroll falls back to the focused path")
        }
        guard BackgroundInput.supportsScroll else {
            return Check(name: "background_scroll", ok: false,
                         detail: "SLEventRecordPointer not resolvable on this macOS; scroll falls back to the focused path")
        }
        guard window != nil, pid != nil else {
            return Check(name: "background_scroll", ok: false,
                         detail: "not probed: no phone window/process (the scroll-record symbol is present)")
        }
        // The probe result backgroundCheck just cached: symbols resolving
        // is worthless when the WindowServer refuses records for this
        // window — the next scroll goes focused then, whatever dlsym said.
        guard Capability.inputMode() != "focused" else {
            return Check(name: "background_scroll", ok: false,
                         detail: "background input probe failed on this host, so scroll uses the focused path (the scroll-record symbol itself resolves)")
        }
        return Check(name: "background_scroll", ok: true,
                     detail: "scroll records post without focus (best-effort: a verified miss falls back to the focused path)")
    }

    private static func rounded(_ v: Double) -> Double { (v * 100).rounded() / 100 }

    /// Text rendering of the same report for `mirrord --doctor`.
    static func printText() -> Never {
        let r = report()
        let checks = (r["checks"] as? [[String: Any]]) ?? []
        for c in checks {
            let name = (c["name"] as? String) ?? "?"
            let ok = (c["ok"] as? Bool) ?? false
            let detail = (c["detail"] as? String) ?? ""
            let pad = String(repeating: " ", count: max(1, 20 - name.count))
            print("\(name)\(pad)\(ok ? "ok  " : "FAIL")  \(detail)")
        }
        print("")
        print("input_mode           \(r["input_mode"] as? String ?? "?")")
        print("background_scroll    \((r["background_scroll"] as? Bool ?? false) ? "yes (best-effort)" : "no")")
        print("capture              \(r["capture"] as? String ?? "?")")
        print("verify               \(r["verify"] as? String ?? "?")")
        let ok = (r["ok"] as? Bool) ?? false
        // Register mirrord in the TCC panes / prompt when something the
        // focused fallback needs is missing, so the report is actionable.
        if !ok {
            if !CGPreflightScreenCaptureAccess() { CGRequestScreenCaptureAccess() }
            if !AXIsProcessTrusted() {
                AXIsProcessTrustedWithOptions(["AXTrustedCheckOptionPrompt": true] as CFDictionary)
            }
        }
        exit(ok ? 0 : 1)
    }
}
