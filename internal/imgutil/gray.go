package imgutil

import "image"

// luma16 returns the Rec. 601 luma of the pixel at (x, y) on a 16-bit
// scale, with allocation-free fast paths for the common decoded types.
func luma16(img image.Image, x, y int) uint64 {
	switch im := img.(type) {
	case *image.RGBA:
		i := im.PixOffset(x, y)
		return luma8(uint64(im.Pix[i]), uint64(im.Pix[i+1]), uint64(im.Pix[i+2]))
	case *image.NRGBA:
		i := im.PixOffset(x, y)
		if a := im.Pix[i+3]; a != 0xff {
			// Premultiply so translucent pixels match the generic path.
			aa := uint64(a)
			return luma8(uint64(im.Pix[i])*aa/0xff, uint64(im.Pix[i+1])*aa/0xff, uint64(im.Pix[i+2])*aa/0xff)
		}
		return luma8(uint64(im.Pix[i]), uint64(im.Pix[i+1]), uint64(im.Pix[i+2]))
	case *image.Gray:
		v := uint64(im.Pix[im.PixOffset(x, y)])
		return v<<8 | v
	default:
		r, g, b, _ := img.At(x, y).RGBA()
		return (19595*uint64(r) + 38470*uint64(g) + 7471*uint64(b)) >> 16
	}
}

// luma8 combines 8-bit channels into 16-bit-scale Rec. 601 luma.
func luma8(r, g, b uint64) uint64 {
	v := (19595*r + 38470*g + 7471*b) >> 16
	return v<<8 | v
}

// GrayDownscale converts img to 8-bit grayscale, scaled (preserving
// aspect ratio) so its longer edge is at most maxDim (<= 0 keeps the
// native size). Each destination pixel box-averages its source region,
// matching Downscale's filter, and luma uses the Rec. 601 weights.
func GrayDownscale(img image.Image, maxDim int) *image.Gray {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return image.NewGray(image.Rect(0, 0, 0, 0))
	}
	long := w
	if h > long {
		long = h
	}
	dw, dh := w, h
	if maxDim > 0 && long > maxDim {
		dw = w * maxDim / long
		dh = h * maxDim / long
		if dw < 1 {
			dw = 1
		}
		if dh < 1 {
			dh = 1
		}
	}
	dst := image.NewGray(image.Rect(0, 0, dw, dh))
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
			var sum, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					sum += luma16(img, sx, sy)
					n++
				}
			}
			dst.Pix[dst.PixOffset(dx, dy)] = uint8(sum / n >> 8)
		}
	}
	return dst
}
