import CoreGraphics
import Foundation
import ScreenCaptureKit

/// ScreenCaptureKit single-frame capture — the supported, non-deprecated
/// in-process path (macOS 14+). `SCContentFilter(desktopIndependentWindow:)`
/// captures exactly one window, occluded or not, without raising it.
///
/// The `SCShareableContent` lookup is the expensive part (tens of ms), so
/// the filter is cached per window id; a capture failure invalidates the
/// cache and is retried once, which is what a window that was closed and
/// reopened looks like.
final class SCKCapture: CaptureBackend {
    let name = "sck"

    /// Capability, not permission: a missing Screen Recording grant
    /// shows up as a failed capture (and is reported by /v1/doctor),
    /// while `CGPreflightScreenCaptureAccess` is known to answer false
    /// on hosts where capture in fact works.
    var available: Bool {
        if #available(macOS 14.0, *) { return true }
        return false
    }

    /// Bridging box for the async SCK APIs: the helper's HTTP loop is
    /// synchronous and single-threaded, so each capture waits on a
    /// semaphore while the Task runs.
    private final class Box<T> {
        private let lock = NSLock()
        private var value: T?
        func set(_ v: T) {
            lock.lock()
            value = v
            lock.unlock()
        }

        func get() -> T? {
            lock.lock()
            defer { lock.unlock() }
            return value
        }
    }

    private let cacheLock = NSLock()
    private var cachedWindowID: CGWindowID?
    private var cachedFilter: Any?

    func capture(window: MirrorWindow.Window) -> CaptureOutcome {
        guard #available(macOS 14.0, *) else {
            return captureFailure("ScreenCaptureKit single-frame capture needs macOS 14+")
        }
        switch captureOnce(window: window, useCache: true) {
        case .success(let img):
            return .success(img)
        case .failure(let first):
            invalidate()
            switch captureOnce(window: window, useCache: false) {
            case .success(let img):
                return .success(img)
            case .failure(let second):
                return captureFailure(first.reason == second.reason
                    ? first.reason
                    : "\(first.reason); retry: \(second.reason)")
            }
        }
    }

    @available(macOS 14.0, *)
    private func captureOnce(window: MirrorWindow.Window, useCache: Bool) -> CaptureOutcome {
        let filter: SCContentFilter
        if useCache, let cached = cachedContentFilter(for: window.id) {
            filter = cached
        } else {
            switch resolveFilter(for: window.id) {
            case .success(let f):
                filter = f
                store(filter: f, for: window.id)
            case .failure(let why):
                return captureFailure(why.reason)
            }
        }
        let config = SCStreamConfiguration()
        let scale = scaleFor(window)
        config.width = max(1, Int((window.w * scale).rounded()))
        config.height = max(1, Int((window.h * scale).rounded()))
        config.showsCursor = false
        config.captureResolution = .best
        let box = Box<CaptureOutcome>()
        let done = DispatchSemaphore(value: 0)
        Task {
            do {
                let img = try await SCScreenshotManager.captureImage(
                    contentFilter: filter, configuration: config)
                box.set(.success(img))
            } catch {
                box.set(captureFailure("SCScreenshotManager: \(error.localizedDescription)"))
            }
            done.signal()
        }
        if done.wait(timeout: .now() + 5) == .timedOut {
            return captureFailure("SCScreenshotManager timed out after 5s")
        }
        return box.get() ?? captureFailure("SCScreenshotManager returned nothing")
    }

    @available(macOS 14.0, *)
    private func resolveFilter(for wid: CGWindowID) -> Result<SCContentFilter, CaptureFailure> {
        let box = Box<Result<SCContentFilter, CaptureFailure>>()
        let done = DispatchSemaphore(value: 0)
        Task {
            do {
                let content = try await SCShareableContent.excludingDesktopWindows(
                    false, onScreenWindowsOnly: true)
                if let win = content.windows.first(where: { $0.windowID == wid }) {
                    box.set(.success(SCContentFilter(desktopIndependentWindow: win)))
                } else {
                    box.set(.failure(CaptureFailure(reason: "window \(wid) is not in the shareable content list")))
                }
            } catch {
                box.set(.failure(CaptureFailure(reason: "SCShareableContent: \(error.localizedDescription)")))
            }
            done.signal()
        }
        if done.wait(timeout: .now() + 5) == .timedOut {
            return .failure(CaptureFailure(reason: "SCShareableContent timed out after 5s"))
        }
        return box.get() ?? .failure(CaptureFailure(reason: "SCShareableContent returned nothing"))
    }

    @available(macOS 14.0, *)
    private func cachedContentFilter(for wid: CGWindowID) -> SCContentFilter? {
        cacheLock.lock()
        defer { cacheLock.unlock() }
        guard cachedWindowID == wid else { return nil }
        return cachedFilter as? SCContentFilter
    }

    private func store(filter: Any, for wid: CGWindowID) {
        cacheLock.lock()
        cachedFilter = filter
        cachedWindowID = wid
        cacheLock.unlock()
    }

    private func invalidate() {
        cacheLock.lock()
        cachedFilter = nil
        cachedWindowID = nil
        cacheLock.unlock()
    }
}
