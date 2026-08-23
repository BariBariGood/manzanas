import CoreGraphics
import Foundation

/// Focus-free (background) input into a window mirrord does not own.
///
/// Mouse events go to the WindowServer as CGS *event records* addressed
/// to the owning process' PSN (`SLPSPostEventRecordTo`, the mechanism
/// yabai uses), which delivers into a window that is neither frontmost
/// nor key — no activation, so the Mac stays usable while an agent
/// drives the phone. Keyboard events need no private API at all:
/// `CGEvent.postToPid` delivers arbitrary keycodes to a background
/// process.
///
/// Deliberately NOT used: `_SLPSSetFrontProcessWithOptions`. It is a
/// real (if lagging) focus mutation, and a process made CGS-front
/// receives keystrokes typed by the human at the keyboard — the
/// input-leak hazard that makes "background" input silently land in the
/// wrong place.
///
/// Every private symbol is resolved with `dlsym` and never linked: a
/// renamed or removed symbol degrades the helper to the focused
/// fallback (`available == false`) instead of failing to launch. Record
/// writes are bounds-checked against the fixed 0xf8-byte buffer, so a
/// layout change cannot corrupt memory — only drop events, which is
/// what the per-action post-condition check exists to catch.
enum BackgroundInput {
    /// CGS event-record types. The numbering matches CGEventType (mouse
    /// down/up/moved/dragged, key down/up), which is how the record's
    /// type field is filled in.
    enum RecordType: UInt32 {
        case leftMouseDown = 1
        case leftMouseUp = 2
        case mouseMoved = 5
        case leftMouseDragged = 6
    }

    /// Process serial number, laid out like Carbon's
    /// `ProcessSerialNumber`. Declared locally so the helper never
    /// depends on a deprecated SDK declaration staying visible to Swift.
    struct PSN {
        var high: UInt32 = 0
        var low: UInt32 = 0
    }

    // Raw pointers: a Swift struct is not representable in a
    // @convention(c) signature, and both parameters are opaque buffers
    // to the callee anyway.
    private typealias PostEventRecordToFn =
        @convention(c) (UnsafeRawPointer, UnsafeRawPointer) -> Int32
    private typealias GetProcessForPIDFn =
        @convention(c) (pid_t, UnsafeMutableRawPointer) -> Int32

    /// SkyLight owns the CGS connection APIs; the symbol is private, so
    /// it is looked up by name and never linked against.
    private static let skyLightPath =
        "/System/Library/PrivateFrameworks/SkyLight.framework/SkyLight"

    /// dlfcn's RTLD_DEFAULT: the macro is not importable into Swift.
    static let rtldDefault = UnsafeMutableRawPointer(bitPattern: -2)

    /// `GetProcessForPID` is a deprecated Carbon call that Swift can no
    /// longer see in the SDK; it lives in ApplicationServices, which
    /// AppKit already pulls in (hence the RTLD_DEFAULT hit) — the
    /// explicit dlopen is the belt to that braces.
    private static let applicationServicesPath =
        "/System/Library/Frameworks/ApplicationServices.framework/ApplicationServices"

    private static let symbols: (post: PostEventRecordToFn, psn: GetProcessForPIDFn)? = {
        () -> (post: PostEventRecordToFn, psn: GetProcessForPIDFn)? in
        guard let handle = dlopen(skyLightPath, RTLD_LAZY),
              let postSym = dlsym(handle, "SLPSPostEventRecordTo"),
              let psnSym = dlsym(rtldDefault, "GetProcessForPID")
                  ?? dlopen(applicationServicesPath, RTLD_LAZY)
                  .flatMap({ dlsym($0, "GetProcessForPID") })
        else { return nil }
        return (unsafeBitCast(postSym, to: PostEventRecordToFn.self),
                unsafeBitCast(psnSym, to: GetProcessForPIDFn.self))
    }()

    /// Whether the private event-record path is reachable at all. False
    /// on any macOS where the symbols moved: callers fall back to the
    /// focused path.
    static var available: Bool { symbols != nil }

    /// Whether the wheel-scroll record path is reachable. Scroll records
    /// are never hand-written: the delta layout is CoreGraphics' own
    /// (`SLEventRecordPointer` exposes the CGEvent's CGS record, deltas
    /// already filled in), so the only extra requirement over the mouse
    /// path is that symbol. A host where it moved degrades scroll to the
    /// focused path, exactly like a missing `SLPSPostEventRecordTo`
    /// degrades everything else.
    ///
    /// Delivery is best-effort even when true: on real hosts accepted
    /// scroll records intermittently land nothing (while taps keep
    /// working), so callers must verify per action and fall back rather
    /// than trust the capability flag.
    static var supportsScroll: Bool { available && recordPointer != nil }

    /// `SLEventRecordPointer(CGEventRef) -> void *`: the CGEvent's own
    /// CGS event record, deltas and type already filled in by
    /// CoreGraphics. Same 0xf8-byte layout the mouse path writes by hand.
    private typealias EventRecordPointerFn =
        @convention(c) (UnsafeRawPointer) -> UnsafeMutableRawPointer?

    private static let recordPointer: EventRecordPointerFn? = {
        guard let handle = dlopen(skyLightPath, RTLD_LAZY),
              let sym = dlsym(handle, "SLEventRecordPointer") else { return nil }
        return unsafeBitCast(sym, to: EventRecordPointerFn.self)
    }()

    // MARK: mouse

    /// The 0xf8-byte CGS mouse event record. Offsets are the yabai
    /// layout (stable Mojave → macOS 26); every write is bounds-checked
    /// so a future layout change can only drop events, never scribble.
    /// `global` is in Mac screen points, `local` in window-local points.
    static func mouseRecord(type: RecordType, wid: CGWindowID,
                            global: CGPoint, local: CGPoint) -> [UInt8]? {
        var buf = [UInt8](repeating: 0, count: 0xf8)
        guard write(UInt32(0xf8), at: 0x04, into: &buf),
              write(type.rawValue, at: 0x08, into: &buf),
              write(global, at: 0x10, into: &buf),
              write(local, at: 0x20, into: &buf),
              write(UInt8(0x10), at: 0x3a, into: &buf),
              write(UInt32(wid), at: 0x3c, into: &buf)
        else { return nil }
        return buf
    }

    private static func write<T>(_ value: T, at offset: Int, into buf: inout [UInt8]) -> Bool {
        let size = MemoryLayout<T>.size
        guard offset >= 0, offset + size <= buf.count else { return false }
        withUnsafeBytes(of: value) { src in
            for i in 0..<size { buf[offset + i] = src[i] }
        }
        return true
    }

    /// Post one mouse record to the window's owning process. Returns
    /// false when the symbols are missing, the PSN cannot be resolved,
    /// or the WindowServer refuses the record — the caller then treats
    /// the action as undelivered (there is no positive delivery signal
    /// in the return value; that is what the capture-delta check is
    /// for).
    static func postMouse(_ type: RecordType, pid: pid_t, wid: CGWindowID,
                          global: CGPoint, local: CGPoint) -> Bool {
        guard let syms = symbols, let record = mouseRecord(
            type: type, wid: wid, global: global, local: local) else { return false }
        var psn = PSN()
        let resolved = withUnsafeMutableBytes(of: &psn) { raw -> Bool in
            guard let base = raw.baseAddress else { return false }
            return syms.psn(pid, base) == 0
        }
        guard resolved else { return false }
        return record.withUnsafeBytes { rec -> Bool in
            guard let recBase = rec.baseAddress else { return false }
            return withUnsafeBytes(of: &psn) { psnRaw -> Bool in
                guard let psnBase = psnRaw.baseAddress else { return false }
                return syms.post(psnBase, recBase) == 0
            }
        }
    }

    // MARK: scroll

    /// Record offsets CoreGraphics leaves unset in a synthesized scroll
    /// event: the window-local point and the target window id. Everything
    /// else (type, global location, the three delta encodings) is filled
    /// in by CGEvent itself, which is the whole point of building the
    /// record through the public API instead of hand-writing it.
    private static let recordLengthOffset = 0x04
    private static let recordLocalPointOffset = 0x20
    private static let recordWindowIDOffset = 0x3c

    /// Post one pixel-unit wheel-scroll record to the window's owning
    /// process. The event is built with the public
    /// `CGEvent(scrollWheelEvent2Source:)` (CoreGraphics fills the delta
    /// fields), then the CGS record behind it is patched with the window
    /// id and window-local point — the two fields CG leaves empty — and
    /// posted with the same `SLPSPostEventRecordTo` the mouse path uses.
    /// Returns false when a symbol is missing, the record is malformed,
    /// the PSN cannot be resolved, or the WindowServer refuses the
    /// record. Acceptance is not delivery: scroll delivery is
    /// intermittent on real hosts, and only the caller's capture-delta
    /// check can vouch for it.
    static func postScroll(wheel: Int32, pid: pid_t, wid: CGWindowID,
                           global: CGPoint, local: CGPoint) -> Bool {
        guard let syms = symbols, let pointer = recordPointer,
              let ev = CGEvent(scrollWheelEvent2Source: nil, units: .pixel,
                               wheelCount: 1, wheel1: wheel, wheel2: 0, wheel3: 0)
        else { return false }
        ev.location = global
        // The record is owned by the event, so every use of it — patching
        // and posting — has to happen while the event is still alive;
        // ARC is otherwise free to release it after its last mention.
        return withExtendedLifetime(ev) { () -> Bool in
            guard let record = pointer(Unmanaged.passUnretained(ev).toOpaque()) else { return false }
            // Bounds-check every patch against the record's own length
            // field, so a future layout change drops the event instead
            // of scribbling.
            let length = Int(record.load(fromByteOffset: recordLengthOffset, as: UInt32.self))
            guard length >= 0xf8,
                  patch(record, length: length, at: recordLocalPointOffset, Double(local.x)),
                  patch(record, length: length, at: recordLocalPointOffset + 8, Double(local.y)),
                  patch(record, length: length, at: recordWindowIDOffset, UInt32(wid))
            else { return false }
            var psn = PSN()
            let resolved = withUnsafeMutableBytes(of: &psn) { raw -> Bool in
                guard let base = raw.baseAddress else { return false }
                return syms.psn(pid, base) == 0
            }
            guard resolved else { return false }
            return withUnsafeBytes(of: &psn) { psnRaw -> Bool in
                guard let psnBase = psnRaw.baseAddress else { return false }
                return syms.post(psnBase, UnsafeRawPointer(record)) == 0
            }
        }
    }

    private static func patch<T>(_ record: UnsafeMutableRawPointer, length: Int,
                                 at offset: Int, _ value: T) -> Bool {
        guard offset >= 0, offset + MemoryLayout<T>.size <= length else { return false }
        record.storeBytes(of: value, toByteOffset: offset, as: T.self)
        return true
    }

    // MARK: keyboard

    /// Post a key event straight to a process with the public
    /// `CGEvent.postToPid`: no focus change, no private API, arbitrary
    /// keycodes. (The same call is useless for mouse events — those are
    /// simply not delivered — hence the record path above.)
    static func postKey(_ code: CGKeyCode, flags: CGEventFlags, keyDown: Bool, pid: pid_t) -> Bool {
        guard let ev = CGEvent(keyboardEventSource: nil, virtualKey: code, keyDown: keyDown)
        else { return false }
        if !flags.isEmpty { ev.flags = flags }
        ev.postToPid(pid)
        return true
    }
}
