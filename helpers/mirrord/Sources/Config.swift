import Foundation

/// Runtime gates for the focus-free input and in-process capture paths.
///
/// Both default to `auto` (use the fast/background path when a startup
/// probe says it works, otherwise degrade), and both have an env escape
/// hatch so a Mac owner can turn the private path off without editing
/// the LaunchAgent's argument list.
struct MirrorConfig {
    enum Toggle: String {
        case auto, on, off
    }

    /// Background (focus-free) input. `off` keeps the historical
    /// activate-then-post behavior; `on` refuses to fall back to it.
    var background: Toggle = .auto
    /// Per-action post-condition verification by capture delta.
    /// `auto` verifies whenever a capture backend is available.
    var verify: Toggle = .auto
    /// Capture backend override ("sck", "cgwindowlist",
    /// "screencapture"); nil = pick the first available in preference
    /// order.
    var captureBackend: String?

    static var shared = MirrorConfig.fromEnvironment()

    /// MIRROR_BACKGROUND=0 / MIRROR_VERIFY=0 (or "off"/"false") disable
    /// the respective path; MIRROR_CAPTURE pins a capture backend.
    ///
    /// An unknown MIRROR_CAPTURE name is ignored with a warning rather
    /// than pinned: pinning a backend that does not exist would leave the
    /// helper with no way to capture at all (every screenshot failing,
    /// verification silently off) with nothing pointing at the typo. The
    /// `--capture` flag rejects the same mistake outright.
    static func fromEnvironment(_ env: [String: String] = ProcessInfo.processInfo.environment)
        -> MirrorConfig {
        var cfg = MirrorConfig()
        if let v = env["MIRROR_BACKGROUND"], disabled(v) { cfg.background = .off }
        if let v = env["MIRROR_VERIFY"], disabled(v) { cfg.verify = .off }
        if let v = env["MIRROR_CAPTURE"], !v.isEmpty {
            let known = Capture.backends.map(\.name)
            if known.contains(v) {
                cfg.captureBackend = v
            } else {
                FileHandle.standardError.write(Data("mirrord: ignoring MIRROR_CAPTURE=\(v): unknown capture backend (known: \(known.joined(separator: ", ")))\n".utf8))
            }
        }
        return cfg
    }

    private static func disabled(_ v: String) -> Bool {
        ["0", "off", "false", "no"].contains(v.lowercased())
    }
}
