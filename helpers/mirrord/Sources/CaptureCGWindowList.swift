import CoreGraphics
import Foundation

/// `CGWindowListCreateImage` capture — the fast compatibility path
/// (~0.6 ms p50 for a mirror-sized window, versus tens of ms for
/// ScreenCaptureKit and 60-150 ms for a `screencapture` subprocess).
///
/// The function is *obsoleted* in the macOS 15+ SDK: Swift refuses to
/// compile a direct call ("was obsoleted in macOS 15.0") even though the
/// symbol is present and fully functional in CoreGraphics. It is
/// therefore resolved with `dlsym`, which doubles as the availability
/// check — the day it really disappears, this backend reports
/// unavailable instead of failing to build.
final class CGWindowListCapture: CaptureBackend {
    let name = "cgwindowlist"

    private typealias CreateImageFn = @convention(c) (
        CGRect, UInt32, CGWindowID, UInt32) -> Unmanaged<CGImage>?

    /// kCGWindowListOptionIncludingWindow.
    private static let optionIncludingWindow: UInt32 = 1 << 3
    /// kCGWindowImageBoundsIgnoreFraming | kCGWindowImageBestResolution.
    private static let imageOptions: UInt32 = (1 << 0) | (1 << 3)

    private static let symbol: CreateImageFn? = { () -> CreateImageFn? in
        guard let sym = dlsym(BackgroundInput.rtldDefault, "CGWindowListCreateImage")
        else { return nil }
        return unsafeBitCast(sym, to: CreateImageFn.self)
    }()

    var available: Bool { CGWindowListCapture.symbol != nil }

    func capture(window: MirrorWindow.Window) -> CaptureOutcome {
        guard let create = CGWindowListCapture.symbol else {
            return captureFailure("CGWindowListCreateImage is not present in CoreGraphics")
        }
        // A null rect means "the window's own bounds"; the window id
        // selects exactly the mirroring window, so an occluded or
        // background window still captures (no raise, no focus change).
        guard let image = create(CGRect.null, CGWindowListCapture.optionIncludingWindow,
                                window.id, CGWindowListCapture.imageOptions) else {
            return captureFailure("CGWindowListCreateImage returned NULL for window \(window.id) (Screen Recording not granted, or the window is gone)")
        }
        return .success(image.takeRetainedValue())
    }
}
