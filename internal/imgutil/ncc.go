package imgutil

import (
	"image"
	"math"
)

// MatchNCC finds the position of tmpl inside frame using normalized
// cross-correlation on grayscale pixels. It returns the top-left corner
// (x, y) of the best match in frame coordinates and the correlation
// score in [-1, 1] (1 = perfect match). To stay fast in pure Go it runs
// a coarse prescan at reduced resolution and then refines the best
// candidates with full-resolution NCC in a small neighborhood.
//
// If tmpl is larger than frame (in either dimension) or degenerate
// (zero area or uniform pixels make NCC undefined at every offset),
// MatchNCC returns (0, 0, 0).
func MatchNCC(frame, tmpl *image.Gray) (x, y int, score float64) {
	fw, fh := frame.Bounds().Dx(), frame.Bounds().Dy()
	tw, th := tmpl.Bounds().Dx(), tmpl.Bounds().Dy()
	if tw <= 0 || th <= 0 || tw > fw || th > fh {
		return 0, 0, 0
	}

	// Pick the prescan factor: quarter-res when the template is big
	// enough to survive it, half-res otherwise, full-res for tiny
	// templates.
	factor := 1
	if tw >= 16 && th >= 16 {
		factor = 4
	} else if tw >= 8 && th >= 8 {
		factor = 2
	}

	var ox, oy int
	if factor == 1 {
		ox, oy, score = matchNCCExhaustive(frame, tmpl)
	} else {
		smallFrame := shrinkGray(frame, factor)
		smallTmpl := shrinkGray(tmpl, factor)
		sx, sy, sScore := matchNCCExhaustive(smallFrame, smallTmpl)
		if sScore == 0 && sx == 0 && sy == 0 {
			// Degenerate at low res (e.g. uniform after shrinking);
			// fall back to the exhaustive full-res search.
			ox, oy, score = matchNCCExhaustive(frame, tmpl)
		} else {
			ox, oy, score = refineNCC(frame, tmpl, sx*factor, sy*factor, factor+1)
		}
	}
	if score == 0 && ox == 0 && oy == 0 {
		return 0, 0, 0
	}
	// The searches work in offsets from Bounds().Min; rebase onto the
	// frame's own coordinate space.
	return frame.Bounds().Min.X + ox, frame.Bounds().Min.Y + oy, score
}

// refineNCC runs full-resolution NCC in a (2*radius+1)² neighborhood
// around the candidate offset (cx, cy) and returns the best position
// and score found.
func refineNCC(frame, tmpl *image.Gray, cx, cy, radius int) (x, y int, score float64) {
	fw, fh := frame.Bounds().Dx(), frame.Bounds().Dy()
	tw, th := tmpl.Bounds().Dx(), tmpl.Bounds().Dy()
	tMean, tDev := grayStats(tmpl)
	if tDev == 0 {
		return 0, 0, 0
	}
	best := math.Inf(-1)
	bx, by := cx, cy
	for oy := cy - radius; oy <= cy+radius; oy++ {
		if oy < 0 || oy+th > fh {
			continue
		}
		for ox := cx - radius; ox <= cx+radius; ox++ {
			if ox < 0 || ox+tw > fw {
				continue
			}
			s := nccAt(frame, tmpl, ox, oy, tMean, tDev)
			if s > best {
				best, bx, by = s, ox, oy
			}
		}
	}
	if math.IsInf(best, -1) {
		return 0, 0, 0
	}
	return bx, by, best
}

// matchNCCExhaustive slides tmpl over every valid offset of frame.
func matchNCCExhaustive(frame, tmpl *image.Gray) (x, y int, score float64) {
	fw, fh := frame.Bounds().Dx(), frame.Bounds().Dy()
	tw, th := tmpl.Bounds().Dx(), tmpl.Bounds().Dy()
	if tw <= 0 || th <= 0 || tw > fw || th > fh {
		return 0, 0, 0
	}
	tMean, tDev := grayStats(tmpl)
	if tDev == 0 {
		return 0, 0, 0
	}
	best := math.Inf(-1)
	bx, by := 0, 0
	for oy := 0; oy+th <= fh; oy++ {
		for ox := 0; ox+tw <= fw; ox++ {
			s := nccAt(frame, tmpl, ox, oy, tMean, tDev)
			if s > best {
				best, bx, by = s, ox, oy
			}
		}
	}
	if math.IsInf(best, -1) {
		return 0, 0, 0
	}
	return bx, by, best
}

// nccAt computes the normalized cross-correlation of tmpl against the
// frame window whose top-left corner is (ox, oy). tMean and tDev are
// tmpl's precomputed mean and sqrt of summed squared deviation.
func nccAt(frame, tmpl *image.Gray, ox, oy int, tMean, tDev float64) float64 {
	tw, th := tmpl.Bounds().Dx(), tmpl.Bounds().Dy()
	n := float64(tw * th)

	var fSum, fSumSq, cross uint64
	for y := 0; y < th; y++ {
		fi := frame.PixOffset(frame.Bounds().Min.X+ox, frame.Bounds().Min.Y+oy+y)
		ti := tmpl.PixOffset(tmpl.Bounds().Min.X, tmpl.Bounds().Min.Y+y)
		for x := 0; x < tw; x++ {
			f := uint64(frame.Pix[fi+x])
			t := uint64(tmpl.Pix[ti+x])
			fSum += f
			fSumSq += f * f
			cross += f * t
		}
	}
	fMean := float64(fSum) / n
	fVar := float64(fSumSq) - n*fMean*fMean
	if fVar <= 0 {
		return 0
	}
	// cross-deviation: sum((f-fMean)*(t-tMean)) = cross - n*fMean*tMean.
	num := float64(cross) - n*fMean*tMean
	return num / (math.Sqrt(fVar) * tDev)
}

// grayStats returns img's pixel mean and the sqrt of its summed squared
// deviation (the NCC denominator half for the template).
func grayStats(img *image.Gray) (mean, dev float64) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	n := float64(w * h)
	var sum, sumSq uint64
	for y := 0; y < h; y++ {
		i := img.PixOffset(img.Bounds().Min.X, img.Bounds().Min.Y+y)
		for x := 0; x < w; x++ {
			v := uint64(img.Pix[i+x])
			sum += v
			sumSq += v * v
		}
	}
	mean = float64(sum) / n
	ss := float64(sumSq) - n*mean*mean
	if ss <= 0 {
		return mean, 0
	}
	return mean, math.Sqrt(ss)
}

// shrinkGray box-averages img down by an integer factor.
func shrinkGray(img *image.Gray, factor int) *image.Gray {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	dw, dh := w/factor, h/factor
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	dst := image.NewGray(image.Rect(0, 0, dw, dh))
	for dy := 0; dy < dh; dy++ {
		for dx := 0; dx < dw; dx++ {
			var sum, n uint64
			for sy := dy * factor; sy < (dy+1)*factor && sy < h; sy++ {
				i := img.PixOffset(img.Bounds().Min.X, img.Bounds().Min.Y+sy)
				for sx := dx * factor; sx < (dx+1)*factor && sx < w; sx++ {
					sum += uint64(img.Pix[i+sx])
					n++
				}
			}
			dst.Pix[dst.PixOffset(dx, dy)] = uint8(sum / n)
		}
	}
	return dst
}
