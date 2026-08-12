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

    /// Recognize text; box centers/origins are capture-image pixels.
    /// Vision's normalized boxes have a bottom-left origin; image pixels
    /// have a top-left origin, hence the flip.
    static func recognize(png: Data, window: MirrorWindow.Window) throws -> [Box] {
        let (imgW, imgH) = try imageSize(png: png)
        let handler = VNImageRequestHandler(data: png, options: [:])
        let request = VNRecognizeTextRequest()
        request.recognitionLevel = .accurate
        try handler.perform([request])
        var out: [Box] = []
        for obs in request.results ?? [] {
            guard let cand = obs.topCandidates(1).first else { continue }
            let bb = obs.boundingBox
            let px = bb.origin.x * Double(imgW)
            let pyTop = (1.0 - bb.origin.y - bb.size.height) * Double(imgH)
            let pw = bb.size.width * Double(imgW)
            let ph = bb.size.height * Double(imgH)
            out.append(Box(text: cand.string,
                           confidence: Double(cand.confidence),
                           x: (px).rounded(toPlaces: 1),
                           y: (pyTop).rounded(toPlaces: 1),
                           w: pw.rounded(toPlaces: 1),
                           h: ph.rounded(toPlaces: 1)))
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
