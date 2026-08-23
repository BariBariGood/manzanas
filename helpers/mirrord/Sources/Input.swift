import CoreGraphics
import Foundation

/// Input into the iPhone Mirroring window, in one of two modes.
///
/// - `background`: CGS event records addressed to the mirroring process
///   (mouse) and `CGEvent.postToPid` (keys). Nothing is activated, so
///   the human keeps their desktop and input cannot land in whatever app
///   happens to be frontmost.
/// - `focused`: the historical path — activate the window, then post
///   CGEvents at the HID tap. Kept as the fallback for hosts where the
///   private mouse-record symbols are gone, for `--background=off`, and
///   for scroll (whose record layout is not implemented).
///
/// Coordinates are Mac screen points; the background path also needs the
/// window-local point, which it derives from the target's window origin.
/// iPhone Mirroring forwards raw HID keycodes to iOS and ignores unicode
/// payloads, so typing goes through real keycodes (US layout);
/// characters outside the map are untypable.
enum Input {
    /// What actually happened, for the response's `mode` field and the
    /// journal: the mode used plus, when it is not the requested one,
    /// why.
    struct Outcome {
        let mode: String
        var note: String?
    }

    // MARK: mouse

    private static func postMouse(_ type: CGEventType, _ x: Double, _ y: Double) {
        let ev = CGEvent(mouseEventSource: nil, mouseType: type,
                         mouseCursorPosition: CGPoint(x: x, y: y),
                         mouseButton: .left)
        ev?.post(tap: .cghidEventTap)
    }

    /// Post one mouse event in the target's mode. Returns false only for
    /// a background post the WindowServer refused (the focused path has
    /// no delivery signal at all).
    private static func post(_ type: CGEventType, _ x: Double, _ y: Double,
                             _ target: MirrorWindow.InputTarget) -> Bool {
        guard target.isBackground, let record = recordType(type) else {
            postMouse(type, x, y)
            return true
        }
        let global = CGPoint(x: x, y: y)
        let local = CGPoint(x: x - target.window.x, y: y - target.window.y)
        return BackgroundInput.postMouse(record, pid: target.pid, wid: target.window.id,
                                         global: global, local: local)
    }

    private static func recordType(_ type: CGEventType) -> BackgroundInput.RecordType? {
        switch type {
        case .mouseMoved: return .mouseMoved
        case .leftMouseDown: return .leftMouseDown
        case .leftMouseUp: return .leftMouseUp
        case .leftMouseDragged: return .leftMouseDragged
        default: return nil
        }
    }

    /// Tap at screen points; durationMs > 0 is a long-press. A
    /// background target whose very first record is refused degrades to
    /// the focused path before anything was delivered, so a gesture is
    /// never half-posted in two modes.
    static func tap(x: Double, y: Double, durationMs: Int = 0,
                    target: MirrorWindow.InputTarget) throws -> Outcome {
        var target = target
        var note: String?
        if !post(.mouseMoved, x, y, target) {
            (target, note) = try degrade(target, why: "the WindowServer refused a background mouse record")
            _ = post(.mouseMoved, x, y, target)
        }
        Thread.sleep(forTimeInterval: 0.1)
        _ = post(.leftMouseDown, x, y, target)
        Thread.sleep(forTimeInterval: durationMs > 0 ? Double(durationMs) / 1000 : 0.06)
        _ = post(.leftMouseUp, x, y, target)
        return Outcome(mode: target.mode, note: note)
    }

    /// Touch-drag (what iOS sees as a swipe). A fast short drag is a
    /// momentum flick; a slow drag barely moves an iOS list.
    static func drag(x1: Double, y1: Double, x2: Double, y2: Double, durationMs: Int,
                     target: MirrorWindow.InputTarget) throws -> Outcome {
        var target = target
        var note: String?
        if !post(.mouseMoved, x1, y1, target) {
            (target, note) = try degrade(target, why: "the WindowServer refused a background mouse record")
            _ = post(.mouseMoved, x1, y1, target)
        }
        let steps = max(6, min(30, durationMs / 25))
        Thread.sleep(forTimeInterval: 0.1)
        _ = post(.leftMouseDown, x1, y1, target)
        for i in 1...steps {
            let t = Double(i) / Double(steps)
            _ = post(.leftMouseDragged, x1 + (x2 - x1) * t, y1 + (y2 - y1) * t, target)
            Thread.sleep(forTimeInterval: Double(durationMs) / 1000 / Double(steps))
        }
        _ = post(.leftMouseUp, x2, y2, target)
        return Outcome(mode: target.mode, note: note)
    }

    /// Wheel-scroll at (x, y). Positive dy scrolls content down the way a
    /// trackpad two-finger-up does; wheel events advance an iOS list
    /// deterministically where a slow touch-drag bounces back.
    ///
    /// On a background target the scroll records go to the mirroring
    /// process without activating it (BackgroundInput.postScroll).
    /// Delivery on that path is best-effort — the WindowServer accepts
    /// records that intermittently move nothing — so the caller must
    /// treat an unverified background scroll as undelivered and retry
    /// focused; this function only reports what it posted. A refused
    /// first record degrades to the focused path before anything was
    /// delivered, like tap/drag.
    static func scroll(x: Double, y: Double, dy: Double,
                       target: MirrorWindow.InputTarget) throws -> Outcome {
        var target = target
        var note: String?
        let steps = 6
        // Clamp: an out-of-range Double→Int32 conversion traps.
        let wheel = Int32(min(max(-dy / Double(steps), -10_000), 10_000))
        if target.isBackground {
            let global = CGPoint(x: x, y: y)
            let local = CGPoint(x: x - target.window.x, y: y - target.window.y)
            if !BackgroundInput.postScroll(wheel: wheel, pid: target.pid,
                                           wid: target.window.id,
                                           global: global, local: local) {
                (target, note) = try degrade(target, why: "the WindowServer refused a background scroll record")
            } else {
                for _ in 1..<steps {
                    Thread.sleep(forTimeInterval: 0.03)
                    _ = BackgroundInput.postScroll(wheel: wheel, pid: target.pid,
                                                   wid: target.window.id,
                                                   global: global, local: local)
                }
                return Outcome(mode: target.mode, note: note)
            }
        }
        postMouse(.mouseMoved, x, y)
        Thread.sleep(forTimeInterval: 0.1)
        for _ in 0..<steps {
            guard let ev = CGEvent(scrollWheelEvent2Source: nil, units: .pixel,
                                   wheelCount: 1, wheel1: wheel,
                                   wheel2: 0, wheel3: 0) else { continue }
            ev.location = CGPoint(x: x, y: y)
            ev.post(tap: .cghidEventTap)
            Thread.sleep(forTimeInterval: 0.03)
        }
        if note == nil, BackgroundInput.available, !BackgroundInput.supportsScroll,
           MirrorConfig.shared.background != .off {
            note = "scroll uses the focused path: SLEventRecordPointer is not resolvable on this macOS"
        }
        return Outcome(mode: target.mode, note: note)
    }

    /// Switch a background target to the focused path mid-action, when
    /// the private path refused the first record. Fails loudly under
    /// `--background=on`: silently taking over the Mac is exactly what
    /// that flag forbids.
    private static func degrade(_ target: MirrorWindow.InputTarget,
                               why: String) throws -> (MirrorWindow.InputTarget, String?) {
        if MirrorConfig.shared.background == .on {
            throw MirrorWindow.HelperError(
                code: "input-refused",
                message: "\(why) and --background=on forbids the focused fallback; no input was sent")
        }
        let focused = try MirrorWindow.focusedTarget()
        return (focused, "\(why); fell back to the focused path")
    }

    // MARK: keyboard

    static let keycodes: [String: CGKeyCode] = [
        "return": 36, "enter": 36, "tab": 48, "space": 49, "delete": 51,
        "backspace": 51, "escape": 53, "esc": 53,
        "left": 123, "right": 124, "down": 125, "up": 126,
        "1": 18, "2": 19, "3": 20, "4": 21, "5": 23, "6": 22, "7": 26,
        "8": 28, "9": 25, "0": 29,
        "a": 0, "s": 1, "d": 2, "f": 3, "h": 4, "g": 5, "z": 6, "x": 7,
        "c": 8, "v": 9, "b": 11, "q": 12, "w": 13, "e": 14, "r": 15,
        "y": 16, "t": 17, "o": 31, "u": 32, "i": 34, "p": 35, "l": 37,
        "j": 38, "k": 40, "n": 45, "m": 46,
        ".": 47, ",": 43, "/": 44, ";": 41, "'": 39, "[": 33, "]": 30,
        "\\": 42, "-": 27, "=": 24, "`": 50,
    ]

    static let modifiers: [String: CGEventFlags] = [
        "cmd": .maskCommand, "shift": .maskShift,
        "alt": .maskAlternate, "option": .maskAlternate,
        "ctrl": .maskControl,
    ]

    /// Shifted character -> unshifted base key.
    static let shifted: [Character: String] = {
        var m: [Character: String] = [
            "!": "1", "@": "2", "#": "3", "$": "4", "%": "5", "^": "6",
            "&": "7", "*": "8", "(": "9", ")": "0", "_": "-", "+": "=",
            ":": ";", "\"": "'", "<": ",", ">": ".", "?": "/", "~": "`",
            "{": "[", "}": "]", "|": "\\",
        ]
        for c in "ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
            m[c] = String(c).lowercased()
        }
        return m
    }()

    /// Keys need no private API to reach a background window:
    /// `CGEvent.postToPid` delivers arbitrary keycodes to a process that
    /// is neither frontmost nor key.
    private static func postKey(_ code: CGKeyCode, flags: CGEventFlags,
                                _ target: MirrorWindow.InputTarget) {
        for down in [true, false] {
            if target.isBackground {
                _ = BackgroundInput.postKey(code, flags: flags, keyDown: down, pid: target.pid)
            } else {
                let ev = CGEvent(keyboardEventSource: nil, virtualKey: code, keyDown: down)
                if !flags.isEmpty { ev?.flags = flags }
                ev?.post(tap: .cghidEventTap)
            }
            Thread.sleep(forTimeInterval: 0.03)
        }
    }

    /// press("return"), press("cmd+1") — Cmd+1 Home, Cmd+2 App Switcher,
    /// Cmd+3 Spotlight inside iPhone Mirroring.
    static func press(combo: String, target: MirrorWindow.InputTarget) throws -> Outcome {
        let parts = combo.lowercased().split(separator: "+").map(String.init)
        guard let key = parts.last, let code = keycodes[key] else {
            throw MirrorWindow.HelperError(code: "bad-request",
                                           message: "unknown key in combo \(combo)")
        }
        var flags: CGEventFlags = []
        for m in parts.dropLast() {
            guard let f = modifiers[m] else {
                throw MirrorWindow.HelperError(code: "bad-request",
                                               message: "unknown modifier in combo \(combo)")
            }
            flags.insert(f)
        }
        postKey(code, flags: flags, target)
        return Outcome(mode: target.mode)
    }

    /// Side-effect-free check that every character has a HID keycode.
    /// Throws "untypable" on emoji/non-US-layout glyphs. Cheap enough to
    /// run before any target resolution, so a doomed request never
    /// activates the mirroring window.
    static func validateTypable(text: String) throws {
        for line in text.split(separator: "\n", omittingEmptySubsequences: false) {
            for ch in line {
                let base = shifted[ch] ?? String(ch).lowercased()
                let key = ch == " " ? "space" : base
                if keycodes[key] == nil {
                    throw MirrorWindow.HelperError(code: "untypable",
                                                   message: "cannot type \(String(ch).debugDescription) via HID keycodes (iPhone Mirroring forwards keycodes, not unicode)")
                }
            }
        }
    }

    /// Type text via raw keycodes. \n presses return. Throws "untypable"
    /// on characters with no keycode (emoji, non-US-layout glyphs).
    static func type(text: String, target: MirrorWindow.InputTarget) throws -> Outcome {
        // Validate the whole string first so a failure never leaves a
        // half-typed field.
        try validateTypable(text: text)
        for (i, line) in text.split(separator: "\n", omittingEmptySubsequences: false).enumerated() {
            if i > 0 { _ = try press(combo: "return", target: target) }
            for ch in line {
                let needsShift = shifted[ch] != nil
                let base = shifted[ch] ?? String(ch).lowercased()
                let key = ch == " " ? "space" : base
                guard let code = keycodes[key] else { continue }
                postKey(code, flags: needsShift ? .maskShift : [], target)
            }
        }
        return Outcome(mode: target.mode)
    }
}
