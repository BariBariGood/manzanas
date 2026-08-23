import CoreGraphics
import Foundation
import ImageIO

/// Why one capture attempt failed, in words a doctor report or error
/// message can quote.
struct CaptureFailure: Error {
    let reason: String
}

/// A capture attempt: one frame, or the reason there is none.
typealias CaptureOutcome = Result<CGImage, CaptureFailure>

func captureFailure(_ reason: String) -> CaptureOutcome {
    .failure(CaptureFailure(reason: reason))
}

/// One way to grab a single frame of a window. Implementations live in
/// their own files (`CaptureSCK`, `CaptureCGWindowList`,
/// `CaptureScreencapture`) and are ordered by preference in
/// `Capture.backends`.
///
/// Contract: capture must never activate, raise, or focus the window —
/// the whole point of the mirror fast path is that the Mac's owner keeps
/// their desktop.
protocol CaptureBackend: AnyObject {
    /// Stable identifier reported in `/v1/status` and `/v1/doctor`.
    var name: String { get }
    /// Cheap capability check: symbols present, OS new enough, grant
    /// plausible. A backend that returns true may still fail a capture.
    var available: Bool { get }
    /// Grab one frame of the window, or nil with the reason it failed.
    func capture(window: MirrorWindow.Window) -> CaptureOutcome
}

/// Capture backend registry: an ordered list plus the selection and
/// PNG-encoding glue every caller shares.
enum Capture {
    /// Preference order. ScreenCaptureKit is the supported,
    /// non-deprecated path; `CGWindowListCreateImage` (dlsym'd — the
    /// symbol is obsoleted in the macOS 15+ SDK and Swift refuses a
    /// direct call) is the fast compatibility path; the `screencapture`
    /// subprocess is a last resort only (process spawn + PNG encode +
    /// file I/O per frame).
    static let backends: [CaptureBackend] = [
        SCKCapture(), CGWindowListCapture(), ScreencaptureCapture(),
    ]

    /// The backend that will service captures, honoring the
    /// `--capture`/`MIRROR_CAPTURE` override.
    static func selected() -> CaptureBackend? {
        if let pinned = MirrorConfig.shared.captureBackend {
            return backends.first { $0.name == pinned && $0.available }
        }
        return backends.first { $0.available }
    }

    /// The backend that served the most recent successful frame.
    /// `available` is a capability answer, not a permission one (SCK is
    /// "available" on any macOS 14+ even when Screen Recording is
    /// denied), so only an actual capture proves who really serves
    /// frames.
    private static let lastLock = NSLock()
    private static var lastServed: String?

    /// Name of the backend actually serving captures, for status
    /// reporting: the one that produced the most recent frame, or —
    /// before any frame has been captured — the preferred candidate
    /// ("none" when the host offers nothing, e.g. a pinned backend that
    /// is unavailable).
    static func activeName() -> String {
        lastLock.lock()
        defer { lastLock.unlock() }
        return lastServed ?? selected()?.name ?? "none"
    }

    /// A backend was pinned that this host cannot run — a configuration
    /// mistake, not a host without capture. Callers must not read the
    /// resulting "no backend" as permission to skip verification: the
    /// pinned backend is still tried (`candidates`) and fails loudly.
    static func pinnedUnavailable() -> String? {
        guard let pinned = MirrorConfig.shared.captureBackend, selected() == nil else { return nil }
        return pinned
    }

    /// Grab one frame, falling through the remaining backends when the
    /// preferred one fails (a missing Screen Recording grant makes SCK
    /// and CGWindowList fail identically, so the fallthrough mostly
    /// buys resilience against a single backend regressing).
    static func frame(window: MirrorWindow.Window) throws -> (image: CGImage, backend: String) {
        var tried: [String] = []
        for backend in candidates() {
            switch backend.capture(window: window) {
            case .success(let img):
                lastLock.lock()
                lastServed = backend.name
                lastLock.unlock()
                return (img, backend.name)
            case .failure(let why):
                tried.append("\(backend.name): \(why.reason)")
            }
        }
        let detail = tried.isEmpty ? "no capture backend available" : tried.joined(separator: "; ")
        throw MirrorWindow.HelperError(
            code: "capture-failed",
            message: "window capture failed (is Screen Recording granted to mirrord?): \(detail)")
    }

    private static func candidates() -> [CaptureBackend] {
        if let pinned = MirrorConfig.shared.captureBackend {
            return backends.filter { $0.name == pinned }
        }
        return backends.filter { $0.available }
    }

    static func png(_ image: CGImage) throws -> Data {
        let out = NSMutableData()
        guard let dest = CGImageDestinationCreateWithData(
            out, "public.png" as CFString, 1, nil) else {
            throw MirrorWindow.HelperError(code: "capture-failed",
                                           message: "cannot create PNG encoder")
        }
        CGImageDestinationAddImage(dest, image, nil)
        guard CGImageDestinationFinalize(dest) else {
            throw MirrorWindow.HelperError(code: "capture-failed",
                                           message: "cannot encode capture as PNG")
        }
        return out as Data
    }
}
