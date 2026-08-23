package imgutil

import "image"

// MeanAbsDiff returns the mean absolute per-pixel difference between a
// and b, normalized to [0, 1] (0 = identical, 1 = full-range change on
// every pixel). Pixels inside any of the masks rectangles (in a's
// coordinate space) are excluded from the comparison — masks carve out
// volatile regions such as clocks or spinners. The images are compared
// over their overlapping width/height; if either dimension does not
// overlap, or every pixel is masked, MeanAbsDiff returns 0.
func MeanAbsDiff(a, b *image.Gray, masks []image.Rectangle) float64 {
	aw, ah := a.Bounds().Dx(), a.Bounds().Dy()
	bw, bh := b.Bounds().Dx(), b.Bounds().Dy()
	w, h := aw, ah
	if bw < w {
		w = bw
	}
	if bh < h {
		h = bh
	}
	if w <= 0 || h <= 0 {
		return 0
	}
	var sum, n uint64
	for y := 0; y < h; y++ {
		ai := a.PixOffset(a.Bounds().Min.X, a.Bounds().Min.Y+y)
		bi := b.PixOffset(b.Bounds().Min.X, b.Bounds().Min.Y+y)
		for x := 0; x < w; x++ {
			if pointMasked(a.Bounds().Min.X+x, a.Bounds().Min.Y+y, masks) {
				continue
			}
			d := int(a.Pix[ai+x]) - int(b.Pix[bi+x])
			if d < 0 {
				d = -d
			}
			sum += uint64(d)
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return float64(sum) / float64(n) / 255
}

func pointMasked(x, y int, masks []image.Rectangle) bool {
	for _, m := range masks {
		if x >= m.Min.X && x < m.Max.X && y >= m.Min.Y && y < m.Max.Y {
			return true
		}
	}
	return false
}
