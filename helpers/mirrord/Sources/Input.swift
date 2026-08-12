import CoreGraphics
import Foundation

/// HID-level input into the iPhone Mirroring window: CGEvents posted at
/// the HID tap. iPhone Mirroring forwards raw HID keycodes to iOS and
/// ignores unicode payloads, so typing goes through real keycodes (US
/// layout); characters outside the map are untypable.
enum Input {
    // MARK: mouse

    private static func postMouse(_ type: CGEventType, _ x: Double, _ y: Double) {
        let ev = CGEvent(mouseEventSource: nil, mouseType: type,
                         mouseCursorPosition: CGPoint(x: x, y: y),
                         mouseButton: .left)
        ev?.post(tap: .cghidEventTap)
    }

    /// Tap at screen points; durationMs > 0 is a long-press.
    static func tap(x: Double, y: Double, durationMs: Int = 0) {
        postMouse(.mouseMoved, x, y)
        Thread.sleep(forTimeInterval: 0.1)
        postMouse(.leftMouseDown, x, y)
        Thread.sleep(forTimeInterval: durationMs > 0 ? Double(durationMs) / 1000 : 0.06)
        postMouse(.leftMouseUp, x, y)
    }

    /// Touch-drag (what iOS sees as a swipe). A fast short drag is a
    /// momentum flick; a slow drag barely moves an iOS list.
    static func drag(x1: Double, y1: Double, x2: Double, y2: Double, durationMs: Int) {
        let steps = max(6, min(30, durationMs / 25))
        postMouse(.mouseMoved, x1, y1)
        Thread.sleep(forTimeInterval: 0.1)
        postMouse(.leftMouseDown, x1, y1)
        for i in 1...steps {
            let t = Double(i) / Double(steps)
            postMouse(.leftMouseDragged, x1 + (x2 - x1) * t, y1 + (y2 - y1) * t)
            Thread.sleep(forTimeInterval: Double(durationMs) / 1000 / Double(steps))
        }
        postMouse(.leftMouseUp, x2, y2)
    }

    /// Wheel-scroll at (x, y). Positive dy scrolls content down the way a
    /// trackpad two-finger-up does; wheel events advance an iOS list
    /// deterministically where a slow touch-drag bounces back.
    static func scroll(x: Double, y: Double, dy: Double) {
        postMouse(.mouseMoved, x, y)
        Thread.sleep(forTimeInterval: 0.1)
        let steps = 6
        for _ in 0..<steps {
            // Clamp: an out-of-range Double→Int32 conversion traps.
            let wheel = Int32(min(max(-dy / Double(steps), -10_000), 10_000))
            guard let ev = CGEvent(scrollWheelEvent2Source: nil, units: .pixel,
                                   wheelCount: 1, wheel1: wheel,
                                   wheel2: 0, wheel3: 0) else { continue }
            ev.location = CGPoint(x: x, y: y)
            ev.post(tap: .cghidEventTap)
            Thread.sleep(forTimeInterval: 0.03)
        }
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

    private static func postKey(_ code: CGKeyCode, flags: CGEventFlags) {
        for down in [true, false] {
            let ev = CGEvent(keyboardEventSource: nil, virtualKey: code, keyDown: down)
            if !flags.isEmpty { ev?.flags = flags }
            ev?.post(tap: .cghidEventTap)
            Thread.sleep(forTimeInterval: 0.03)
        }
    }

    /// press("return"), press("cmd+1") — Cmd+1 Home, Cmd+2 App Switcher,
    /// Cmd+3 Spotlight inside iPhone Mirroring.
    static func press(combo: String) throws {
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
        postKey(code, flags: flags)
    }

    /// Type text via raw keycodes. \n presses return. Throws "untypable"
    /// on characters with no keycode (emoji, non-US-layout glyphs).
    static func type(text: String) throws {
        // Validate the whole string first so a failure never leaves a
        // half-typed field.
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
        for (i, line) in text.split(separator: "\n", omittingEmptySubsequences: false).enumerated() {
            if i > 0 { try press(combo: "return") }
            for ch in line {
                let needsShift = shifted[ch] != nil
                let base = shifted[ch] ?? String(ch).lowercased()
                let key = ch == " " ? "space" : base
                guard let code = keycodes[key] else { continue }
                postKey(code, flags: needsShift ? .maskShift : [])
            }
        }
    }
}
