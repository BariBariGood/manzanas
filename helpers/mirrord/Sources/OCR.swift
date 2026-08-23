import CoreGraphics
import Foundation
import ImageIO
import Vision

/// Vision-framework text recognition over window captures — the mirror
/// backend's element tree. Every visible string gets a bounding box in
/// capture-image pixels (top-left origin), the shared coordinate space
/// for screenshots and input.
enum OCR {
    struct Box {
        let text: String
        let confidence: Double
        let x: Double
        let y: Double
        let w: Double
        let h: Double

        var json: [String: Any] {
            ["text": text, "confidence": confidence, "x": x, "y": y, "w": w, "h": h]
        }
    }

    /// Options for one recognition pass, parsed from the /v1/ocr POST
    /// body. The default value reproduces the legacy GET behavior:
    /// accurate recognition over the full frame, no downscale.
    struct Options {
        var level: VNRequestTextRecognitionLevel = .accurate
        var levelName: String = "accurate"
        /// Region of interest in capture-image pixels (top-left origin).
        /// Boxes are still reported in full-capture coordinates.
        var rect: CGRect?
        /// Cap the long edge at this many pixels before recognition
        /// (0 = no cap). Frames at or below `minDownscaleWidth` wide are
        /// never downscaled.
        var maxDim: Int = 0
    }

    /// Frames at or below this width are never downscaled: mirror
    /// captures are ~318px wide and any further reduction destroys
    /// recognition.
    static let minDownscaleWidth = 640

    static func imageSize(png: Data) throws -> (Int, Int) {
        guard let src = CGImageSourceCreateWithData(png as CFData, nil),
              let props = CGImageSourceCopyPropertiesAtIndex(src, 0, nil) as? [String: Any],
              let w = props[kCGImagePropertyPixelWidth as String] as? Int,
              let h = props[kCGImagePropertyPixelHeight as String] as? Int
        else {
            throw MirrorWindow.HelperError(code: "capture-failed", message: "cannot read capture image")
        }
        return (w, h)
    }

    static func decode(png: Data) throws -> CGImage {
        guard let src = CGImageSourceCreateWithData(png as CFData, nil),
              let img = CGImageSourceCreateImageAtIndex(src, 0, nil)
        else {
            throw MirrorWindow.HelperError(code: "capture-failed", message: "cannot decode capture image")
        }
        return img
    }

    /// Recognize text; box origins are full-capture-image pixels even
    /// when a rect crop or downscale was applied. Vision's normalized
    /// boxes have a bottom-left origin; image pixels have a top-left
    /// origin, hence the flip. Returns the boxes plus the scale actually
    /// served to recognition (1.0 = full resolution).
    static func recognize(png: Data, window: MirrorWindow.Window,
                          options: Options = Options()) throws -> (boxes: [Box], servedScale: Double) {
        var img = try decode(png: png)
        var offX = 0.0
        var offY = 0.0
        if let r = options.rect {
            let bounds = CGRect(x: 0, y: 0, width: img.width, height: img.height)
            let clipped = r.intersection(bounds).integral
            guard clipped.width >= 1, clipped.height >= 1, let cropped = img.cropping(to: clipped) else {
                throw MirrorWindow.HelperError(
                    code: "bad-request",
                    message: "rect (\(r.origin.x), \(r.origin.y), \(r.width)x\(r.height)) lies outside the \(img.width)x\(img.height) px capture")
            }
            offX = clipped.origin.x
            offY = clipped.origin.y
            img = cropped
        }
        let croppedW = Double(img.width)
        let longEdge = max(img.width, img.height)
        if options.maxDim > 0, longEdge > options.maxDim, img.width > minDownscaleWidth {
            var scale = Double(options.maxDim) / Double(longEdge)
            // Never shrink below the width floor even when max_dim asks.
            let floorScale = Double(minDownscaleWidth) / croppedW
            if scale < floorScale { scale = floorScale }
            img = try downscale(img, by: scale)
        }
        let workW = Double(img.width)
        let workH = Double(img.height)
        let servedScale = workW / croppedW
        let handler = VNImageRequestHandler(cgImage: img, options: [:])
        let request = VNRecognizeTextRequest()
        request.recognitionLevel = options.level
        try handler.perform([request])
        var out: [Box] = []
        for obs in request.results ?? [] {
            guard let cand = obs.topCandidates(1).first else { continue }
            let bb = obs.boundingBox
            let px = bb.origin.x * workW / servedScale + offX
            let pyTop = (1.0 - bb.origin.y - bb.size.height) * workH / servedScale + offY
            let pw = bb.size.width * workW / servedScale
            let ph = bb.size.height * workH / servedScale
            out.append(Box(text: cand.string,
                           confidence: Double(cand.confidence),
                           x: px.rounded(toPlaces: 1),
                           y: pyTop.rounded(toPlaces: 1),
                           w: pw.rounded(toPlaces: 1),
                           h: ph.rounded(toPlaces: 1)))
        }
        return (out, (servedScale * 1000).rounded() / 1000)
    }

    /// Fraction of the frame's top reserved for the phone's status bar
    /// (clock, battery, carrier): its text ticks on its own schedule and
    /// must never count as evidence that a keystroke landed.
    static let statusBarFraction = 0.05

    /// Normalized recognized-text signature of a capture, for text-delta
    /// verification of keyboard input. Typed glyphs are a handful of
    /// pixels in a full frame — near the pixel-delta noise floor — but
    /// they change what the text recognizer reads, which video noise
    /// cannot. Kept deliberately hard to satisfy by accident: status-bar
    /// text is excluded, low-confidence candidates (recognition jitter on
    /// an unchanged screen) are dropped, and strings are trimmed and
    /// lowercased so a re-recognition of the same glyphs compares equal.
    /// The caller verifies on *additions* to this set only, not on any
    /// inequality. nil when recognition itself failed (distinct from
    /// "recognized nothing", which is a valid empty signature).
    static func textSignature(_ img: CGImage) -> Set<String>? {
        let handler = VNImageRequestHandler(cgImage: img, options: [:])
        let request = VNRecognizeTextRequest()
        request.recognitionLevel = .accurate
        guard (try? handler.perform([request])) != nil else { return nil }
        var out: Set<String> = []
        for obs in request.results ?? [] {
            guard let cand = obs.topCandidates(1).first, cand.confidence >= 0.3 else { continue }
            // boundingBox is normalized with a bottom-left origin: the
            // top edge of the box is origin.y + height.
            let bb = obs.boundingBox
            if bb.origin.y + bb.size.height >= 1.0 - statusBarFraction { continue }
            let t = cand.string.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
            if !t.isEmpty { out.insert(t) }
        }
        return out
    }

    private static func downscale(_ img: CGImage, by scale: Double) throws -> CGImage {
        let w = max(1, Int((Double(img.width) * scale).rounded()))
        let h = max(1, Int((Double(img.height) * scale).rounded()))
        guard let ctx = CGContext(
            data: nil, width: w, height: h, bitsPerComponent: 8, bytesPerRow: 0,
            space: CGColorSpaceCreateDeviceRGB(),
            bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)
        else {
            throw MirrorWindow.HelperError(code: "capture-failed", message: "cannot downscale capture image")
        }
        ctx.interpolationQuality = .high
        ctx.draw(img, in: CGRect(x: 0, y: 0, width: w, height: h))
        guard let out = ctx.makeImage() else {
            throw MirrorWindow.HelperError(code: "capture-failed", message: "cannot downscale capture image")
        }
        return out
    }
}

extension Double {
    func rounded(toPlaces places: Int) -> Double {
        let f = pow(10.0, Double(places))
        return (self * f).rounded() / f
    }
}
