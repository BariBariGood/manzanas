import CoreGraphics
import Foundation

/// Pixel statistics over window captures — the evidence behind
/// `verified: true`.
///
/// Input into a video-stream window has no return code worth trusting
/// (the record poster reports "accepted", not "delivered", and the
/// focused path reports nothing at all), so every action is checked by
/// comparing a frame taken before it with a frame taken after it. A
/// uniformity measure over a single frame doubles as the doctor's
/// "captured, but blank" probe: a real frame scores well above zero, a
/// black one scores zero.
enum PixelDelta {
    /// Default sampling grid for comparison and uniformity. Small on
    /// purpose: this runs on the helper's only thread, between a gesture
    /// and its response. Callers checking a whole frame for a small
    /// change (a few glyphs in a text field) pass a finer grid, so the
    /// change is not averaged away by the downscale.
    private static let side = 64

    /// A pixel changed when any sampled luma differs by more than this
    /// (8-bit levels) — above sensor/encoder noise in a mirrored video
    /// stream, below any real UI state change.
    private static let changeThreshold = 8.0

    /// Fraction (0...1) of sampled pixels inside `region` that changed
    /// between the two frames. nil when the comparison cannot be made:
    /// either frame unsampleable, or the two frames differing in size —
    /// a resized window or a fallthrough to a backend that sizes frames
    /// differently proves nothing about whether input landed, and
    /// scoring it as fully changed would manufacture evidence.
    static func changedFraction(_ before: CGImage, _ after: CGImage, region: CGRect?,
                                grid: Int = side) -> Double? {
        guard before.width == after.width, before.height == after.height else { return nil }
        guard let a = sample(before, region: region, grid: grid),
              let b = sample(after, region: region, grid: grid),
              a.count == b.count, !a.isEmpty else { return nil }
        var changed = 0
        for i in 0..<a.count where abs(Double(a[i]) - Double(b[i])) > changeThreshold {
            changed += 1
        }
        return Double(changed) / Double(a.count)
    }

    /// Mean absolute deviation of a frame's sampled luma. 0 means every
    /// sampled pixel is identical (a black/blank capture).
    static func meanAbsoluteDeviation(_ img: CGImage) -> Double? {
        guard let px = sample(img, region: nil), !px.isEmpty else { return nil }
        let mean = px.reduce(0.0) { $0 + Double($1) } / Double(px.count)
        return px.reduce(0.0) { $0 + abs(Double($1) - mean) } / Double(px.count)
    }

    /// Draw (a crop of) the frame into a fixed-size 8-bit gray buffer.
    private static func sample(_ img: CGImage, region: CGRect?, grid: Int = side) -> [UInt8]? {
        let side = max(8, grid)
        var src = img
        if let r = region {
            let bounds = CGRect(x: 0, y: 0, width: img.width, height: img.height)
            let clipped = r.intersection(bounds).integral
            guard clipped.width >= 1, clipped.height >= 1,
                  let cropped = img.cropping(to: clipped) else { return nil }
            src = cropped
        }
        var buf = [UInt8](repeating: 0, count: side * side)
        let ok = buf.withUnsafeMutableBytes { raw -> Bool in
            guard let base = raw.baseAddress,
                  let ctx = CGContext(data: base, width: side, height: side,
                                     bitsPerComponent: 8, bytesPerRow: side,
                                     space: CGColorSpaceCreateDeviceGray(),
                                     bitmapInfo: CGImageAlphaInfo.none.rawValue)
            else { return false }
            ctx.interpolationQuality = .low
            ctx.draw(src, in: CGRect(x: 0, y: 0, width: side, height: side))
            return true
        }
        return ok ? buf : nil
    }
}
