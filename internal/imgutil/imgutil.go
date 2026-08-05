// Package imgutil provides the shared server-side frame transforms:
// downscaling and JPEG re-encoding used by the screenshot action and the
// media streamer to shrink multi-MB captures before they cross the wire.
package imgutil

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
)

// DefaultJPEGQuality is used when a transform forces a re-encode but the
// caller did not pick a quality.
const DefaultJPEGQuality = 80

// Transcode decodes src (PNG or JPEG), downscales it so its longer edge is
// at most maxDim (0 = keep native size), and re-encodes it as format
// ("png" or "jpeg"). quality applies to JPEG (1-100; 0 = default).
func Transcode(src []byte, format string, quality, maxDim int) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	img = Downscale(img, maxDim)
	var buf bytes.Buffer
	switch format {
	case "png":
		err = png.Encode(&buf, img)
	case "jpeg":
		if quality <= 0 {
			quality = DefaultJPEGQuality
		}
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
	default:
		return nil, fmt.Errorf("unsupported image format %q", format)
	}
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", format, err)
	}
	return buf.Bytes(), nil
}

// Downscale returns img scaled (preserving aspect ratio) so its longer
// edge is at most maxDim. maxDim <= 0 or an already-small image returns
// img unchanged. Each destination pixel averages its source box, which is
// the right filter for the large shrink factors used here.
func Downscale(img image.Image, maxDim int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	long := w
	if h > long {
		long = h
	}
	if maxDim <= 0 || long <= maxDim {
		return img
	}
	dw := w * maxDim / long
	dh := h * maxDim / long
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for dy := 0; dy < dh; dy++ {
		sy0 := b.Min.Y + dy*h/dh
		sy1 := b.Min.Y + (dy+1)*h/dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := 0; dx < dw; dx++ {
			sx0 := b.Min.X + dx*w/dw
			sx1 := b.Min.X + (dx+1)*w/dw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var r, g, bl, a, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					pr, pg, pb, pa := img.At(sx, sy).RGBA()
					r += uint64(pr)
					g += uint64(pg)
					bl += uint64(pb)
					a += uint64(pa)
					n++
				}
			}
			i := dst.PixOffset(dx, dy)
			dst.Pix[i+0] = uint8(r / n >> 8)
			dst.Pix[i+1] = uint8(g / n >> 8)
			dst.Pix[i+2] = uint8(bl / n >> 8)
			dst.Pix[i+3] = uint8(a / n >> 8)
		}
	}
	return dst
}
