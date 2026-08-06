// simbridge: a resident warm-action helper for manzanasd.
//
// One process serves one simulator (--udid). It loads FBSimulatorControl's
// private frameworks and connects to the simulator ONCE at startup, then
// services JSON-lines requests on stdin, writing one JSON response line to
// stdout per request. manzanasd's warm actions backend keeps one simbridge
// per leased simulator, eliminating the per-command process spawn +
// framework bootstrap of the cold AXe CLI path (~1s+ per action).
//
// Protocol (v1, line-delimited JSON):
//   startup:  {"event":"ready","udid":"...","version":1}
//   request:  {"id":"1","op":"tap","args":{"x":100,"y":200}}
//   response: {"id":"1","ok":true,"result":{...}}
//           | {"id":"1","ok":false,"code":"bad_request","error":"..."}
// Ops: ping, tap, swipe, button, key, key_sequence, key_combo, describe_ui.
//
// Build: see build.sh (links against the FBSimulatorControl frameworks
// shipped with an AXe install, MIT-licensed).

import Foundation
import FBControlCore
import FBSimulatorControl

// MARK: - Wire types

struct Request: Decodable {
    let id: String
    let op: String
    let args: [String: JSONValue]?
}

enum JSONValue: Decodable {
    case number(Double)
    case string(String)
    case bool(Bool)
    case array([JSONValue])
    case null

    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null }
        else if let b = try? c.decode(Bool.self) { self = .bool(b) }
        else if let n = try? c.decode(Double.self) { self = .number(n) }
        else if let s = try? c.decode(String.self) { self = .string(s) }
        else if let a = try? c.decode([JSONValue].self) { self = .array(a) }
        else { throw DecodingError.dataCorruptedError(in: c, debugDescription: "unsupported JSON value") }
    }

    var double: Double? { if case .number(let n) = self { return n }; return nil }
    var string: String? { if case .string(let s) = self { return s }; return nil }
    var doubles: [Double]? {
        if case .array(let a) = self { return a.compactMap { $0.double } }
        return nil
    }
}

struct BridgeError: Error {
    let code: String
    let message: String
    static func badRequest(_ m: String) -> BridgeError { BridgeError(code: "bad_request", message: m) }
    static func internalError(_ m: String) -> BridgeError { BridgeError(code: "internal", message: m) }
}

// MARK: - Output (single-writer stdout)

let stdoutLock = NSLock()
func emit(_ obj: [String: Any]) {
    guard let data = try? JSONSerialization.data(withJSONObject: obj) else { return }
    stdoutLock.lock()
    defer { stdoutLock.unlock() }
    FileHandle.standardOutput.write(data)
    FileHandle.standardOutput.write(Data([0x0A]))
}

func emitResult(id: String, result: [String: Any]) {
    emit(["id": id, "ok": true, "result": result])
}

func emitError(id: String, code: String, message: String) {
    emit(["id": id, "ok": false, "code": code, "error": message])
}

// MARK: - Simulator session

final class QuietLogger: NSObject, FBControlCoreLogger {
    var level: FBControlCoreLogLevel { .error }
    var name: String? { nil }
    func log(_ message: String) -> FBControlCoreLogger { self }
    func logFormat(_ format: String, _ args: CVarArg...) -> FBControlCoreLogger { self }
    func info() -> FBControlCoreLogger { self }
    func debug() -> FBControlCoreLogger { self }
    func error() -> FBControlCoreLogger { self }
    func withName(_ name: String) -> FBControlCoreLogger { self }
    func withDateFormatEnabled(_ enabled: Bool) -> FBControlCoreLogger { self }
}

final class QuietReporter: NSObject, FBEventReporter {
    var metadata: [String: String] = [:]
    func report(_ subject: FBEventReporterSubject) {}
    func addMetadata(_ metadata: [String: String]) {}
}

@MainActor
final class Bridge {
    let udid: String
    let simulator: FBSimulator
    var hid: FBSimulatorHID?

    init(udid: String) async throws {
        self.udid = udid
        let logger = QuietLogger()
        try FBSimulatorControlFrameworkLoader.essentialFrameworks.loadPrivateFrameworks(logger)
        try FBSimulatorControlFrameworkLoader.xcodeFrameworks.loadPrivateFrameworks(logger)
        // Some CoreSimulator paths (including HID) key off the global logger's
        // client identity; leave it set exactly as AXe does.
        FBControlCoreGlobalConfiguration.defaultLogger = logger
        let config = FBSimulatorControlConfiguration(deviceSetPath: nil, logger: logger, reporter: QuietReporter())
        let control = try FBSimulatorControl.withConfiguration(config)
        guard let sim = control.set.allSimulators.first(where: { $0.udid == udid }) else {
            throw BridgeError.badRequest("simulator \(udid) not found")
        }
        guard sim.state == .booted else {
            throw BridgeError.badRequest("simulator \(udid) is not booted")
        }
        self.simulator = sim
    }

    func connectedHID() async throws -> FBSimulatorHID {
        if let hid { return hid }
        let h = try await simulator.connectToHID()
        hid = h
        return h
    }

    func send(_ event: FBSimulatorHIDEvent) async throws {
        let h = try await connectedHID()
        do {
            try await h.send(event: event, logger: QuietLogger())
        } catch {
            // The HID connection dies when the simulator reboots; drop it so
            // the next action reconnects instead of failing forever.
            hid = nil
            throw error
        }
        // Small stabilization delay so back-to-back events all register.
        try await Task.sleep(nanoseconds: 25_000_000)
    }

    func handle(_ req: Request) async throws -> [String: Any] {
        let a = req.args ?? [:]
        switch req.op {
        case "ping":
            return ["pong": true]
        case "tap":
            guard let x = a["x"]?.double, let y = a["y"]?.double else {
                throw BridgeError.badRequest("tap requires numeric x and y")
            }
            try await send(.tapAt(x: x, y: y))
            return ["x": x, "y": y]
        case "swipe":
            guard let sx = a["start_x"]?.double, let sy = a["start_y"]?.double,
                  let ex = a["end_x"]?.double, let ey = a["end_y"]?.double else {
                throw BridgeError.badRequest("swipe requires start_x, start_y, end_x, end_y")
            }
            let duration = a["duration"]?.double ?? 0.5
            try await send(.swipe(sx, yStart: sy, xEnd: ex, yEnd: ey, delta: 10, duration: duration))
            return ["start_x": sx, "start_y": sy, "end_x": ex, "end_y": ey]
        case "button":
            guard let name = a["name"]?.string, let button = buttonType(name) else {
                throw BridgeError.badRequest("unknown button")
            }
            try await send(.shortButtonPress(button))
            return ["button": name]
        case "key":
            guard let raw = a["keycode"]?.double, let code = UInt32(exactly: raw) else {
                throw BridgeError.badRequest("key requires an integer keycode in 0...UInt32.max")
            }
            if let duration = a["duration"]?.double, duration > 0 {
                try await send(.composite([
                    .keyboard(direction: .down, keyCode: code),
                    .delay(duration),
                    .keyboard(direction: .up, keyCode: code),
                ]))
            } else {
                try await send(.shortKeyPress(code))
            }
            return ["keycode": Int(code)]
        case "key_sequence":
            guard let raws = a["keycodes"]?.doubles, !raws.isEmpty else {
                throw BridgeError.badRequest("key_sequence requires keycodes")
            }
            let codes = try raws.map { raw -> UInt32 in
                guard let code = UInt32(exactly: raw) else {
                    throw BridgeError.badRequest("keycodes must be integers in 0...UInt32.max")
                }
                return code
            }
            var events: [FBSimulatorHIDEvent] = []
            for (i, code) in codes.enumerated() {
                events.append(.shortKeyPress(code))
                if i < codes.count - 1 { events.append(.delay(0.05)) }
            }
            try await send(.composite(events))
            return ["count": codes.count]
        case "key_combo":
            // Chorded shortcut: hold the modifiers in order, press the key,
            // release the modifiers in reverse (e.g. Cmd-V for the daemon's
            // paste typing strategy).
            guard let rawMods = a["modifiers"]?.doubles, !rawMods.isEmpty else {
                throw BridgeError.badRequest("key_combo requires a non-empty modifiers array")
            }
            guard let rawKey = a["keycode"]?.double, let key = UInt32(exactly: rawKey) else {
                throw BridgeError.badRequest("key_combo requires an integer keycode in 0...UInt32.max")
            }
            let mods = try rawMods.map { raw -> UInt32 in
                guard let code = UInt32(exactly: raw) else {
                    throw BridgeError.badRequest("modifiers must be integers in 0...UInt32.max")
                }
                return code
            }
            var events: [FBSimulatorHIDEvent] = []
            for m in mods { events.append(.keyboard(direction: .down, keyCode: m)) }
            events.append(.delay(0.05))
            events.append(.shortKeyPress(key))
            events.append(.delay(0.05))
            for m in mods.reversed() { events.append(.keyboard(direction: .up, keyCode: m)) }
            try await send(.composite(events))
            return ["keycode": Int(key), "modifiers": mods.map { Int($0) }]
        case "describe_ui":
            let element = try await simulator.accessibilityElementForFrontmostApplication()
            defer { element.close() }
            let response = try element.serialize(
                with: FBAccessibilityRequestOptions(nestedFormat: true, keys: Self.accessibilityKeys)
            )
            let data = try JSONSerialization.data(withJSONObject: response.elements)
            guard let raw = String(data: data, encoding: .utf8) else {
                throw BridgeError.internalError("accessibility tree is not valid UTF-8")
            }
            return ["raw": raw]
        default:
            throw BridgeError.badRequest("unknown op \(req.op)")
        }
    }

    // The same key subset AXe requests: Xcode 27's serializer returns an
    // empty hierarchy when .traits is included.
    static let accessibilityKeys: Set<FBAXKeys> = [
        .label, .frame, .value, .uniqueID, .type, .title, .frameDict, .help,
        .enabled, .customActions, .role, .roleDescription, .subrole,
        .contentRequired, .pid,
    ]
}

// Raw values follow FBSimulatorHIDButton's enum: 1=applePay, 2=home,
// 3=lock, 4=sideButton, 5=siri (matching AXe's mapping).
func buttonType(_ name: String) -> FBSimulatorHIDButton? {
    switch name {
    case "apple-pay": return FBSimulatorHIDButton(rawValue: 1)
    case "home": return FBSimulatorHIDButton(rawValue: 2)
    case "lock": return FBSimulatorHIDButton(rawValue: 3)
    case "side-button": return FBSimulatorHIDButton(rawValue: 4)
    case "siri": return FBSimulatorHIDButton(rawValue: 5)
    default: return nil
    }
}

// MARK: - Main loop

@main
struct Simbridge {
    static func main() async {
        var udid: String?
        var args = ArraySlice(CommandLine.arguments.dropFirst())
        while let arg = args.popFirst() {
            if arg == "--udid" { udid = args.popFirst() }
        }
        guard let udid else {
            emit(["ok": false, "code": "bad_request", "error": "usage: simbridge --udid <UDID>"])
            exit(2)
        }

        let bridge: Bridge
        do {
            bridge = try await Bridge(udid: udid)
        } catch let e as BridgeError {
            emit(["ok": false, "code": e.code, "error": e.message])
            exit(1)
        } catch {
            emit(["ok": false, "code": "internal", "error": "startup: \(error.localizedDescription)"])
            exit(1)
        }
        emit(["event": "ready", "udid": udid, "version": 1])

        // Exit when the simulator leaves the booted state (lease-end
        // reset, erase, reboot): the daemon sees the dead process as a
        // transport failure and bootstraps a fresh helper instead of
        // acting on stale device state.
        let sim = bridge.simulator
        Task.detached {
            while true {
                try? await Task.sleep(nanoseconds: 2_000_000_000)
                if sim.state != .booted {
                    exit(3)
                }
            }
        }

        let decoder = JSONDecoder()
        while let line = readLine(strippingNewline: true) {
            guard !line.isEmpty else { continue }
            guard let data = line.data(using: .utf8),
                  let req = try? decoder.decode(Request.self, from: data) else {
                emit(["ok": false, "code": "bad_request", "error": "malformed request line"])
                continue
            }
            do {
                let result = try await bridge.handle(req)
                emitResult(id: req.id, result: result)
            } catch let e as BridgeError {
                emitError(id: req.id, code: e.code, message: e.message)
            } catch {
                emitError(id: req.id, code: "internal", message: error.localizedDescription)
            }
        }
    }
}
