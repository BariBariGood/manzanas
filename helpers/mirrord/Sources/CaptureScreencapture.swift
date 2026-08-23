import CoreGraphics
import Foundation

/// `/usr/sbin/screencapture` capture — kept only as a last resort for
/// hosts where neither in-process path works (and because it is a
/// different TCC subject, so it occasionally succeeds where the
/// in-process paths were denied).
///
/// It costs a process spawn, a PNG encode and file I/O per frame, so it
/// is last in `Capture.backends`. Unlike the old implementation it never
/// activates the mirroring window: capture must not steal focus.
final class ScreencaptureCapture: CaptureBackend {
    let name = "screencapture"

    private let tool = "/usr/sbin/screencapture"

    var available: Bool { FileManager.default.isExecutableFile(atPath: tool) }

    func capture(window: MirrorWindow.Window) -> CaptureOutcome {
        let path = NSTemporaryDirectory() + "mirrord-capture.png"
        let region = "\(Int(window.x)),\(Int(window.y)),\(Int(window.w)),\(Int(window.h))"
        // -x: no sound, -o: no window shadow. The window form grabs just
        // the mirroring window and works while it is occluded or behind
        // other windows.
        var attempts = [["-x", "-o", "-l", String(window.id), path]]
        // The region form grabs the composited screen rectangle, so it
        // returns whatever is on top of that rectangle. The old code made
        // it safe by activating the window first; capture no longer
        // activates anything, so it is only sound when the mirroring
        // window already owns those pixels — otherwise a neighbouring
        // window's content would be returned as the phone's screen, and
        // the size check would not notice.
        if MirrorWindow.isFrontmost() {
            attempts.append(["-x", "-R", region, path])
        }
        var lastErr = "empty capture"
        for args in attempts {
            switch run(args, path: path) {
            case .success(let img):
                return .success(img)
            case .failure(let why):
                lastErr = why.reason
            }
        }
        return captureFailure(lastErr)
    }

    private func run(_ args: [String], path: String) -> CaptureOutcome {
        try? FileManager.default.removeItem(atPath: path)
        let p = Process()
        p.executableURL = URL(fileURLWithPath: tool)
        p.arguments = args
        let errPipe = Pipe()
        p.standardError = errPipe
        do { try p.run() } catch { return captureFailure("\(error)") }
        p.waitUntilExit()
        let errData = errPipe.fileHandleForReading.readDataToEndOfFile()
        let stderr = String(data: errData, encoding: .utf8) ?? ""
        let size = (try? FileManager.default.attributesOfItem(atPath: path))?[.size] as? Int ?? 0
        guard p.terminationStatus == 0, size > 1000,
              let data = try? Data(contentsOf: URL(fileURLWithPath: path)),
              let img = try? OCR.decode(png: data) else {
            let trimmed = stderr.trimmingCharacters(in: .whitespacesAndNewlines)
            return captureFailure(trimmed.isEmpty ? "empty capture" : trimmed)
        }
        return .success(img)
    }
}
