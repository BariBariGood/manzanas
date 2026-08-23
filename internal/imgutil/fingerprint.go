package imgutil

import (
	"image"
	"math/bits"
)

// DHash computes a 64-bit difference hash of img: the image is
// grayscaled and box-averaged onto a 9x8 grid, and each bit records
// whether a cell is brighter than its right neighbor. Pixels inside any
// of the masks rectangles (in img's coordinate space) are excluded from
// the cell averages, so volatile regions (clock, spinners) do not
// perturb the fingerprint; a fully-masked cell averages to 0.
func DHash(img image.Image, masks []image.Rectangle) uint64 {
	const gw, gh = 9, 8
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return 0
	}
	var cells [gh][gw]uint64
	for cy := 0; cy < gh; cy++ {
		sy0 := cy * h / gh
		sy1 := (cy + 1) * h / gh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for cx := 0; cx < gw; cx++ {
			sx0 := cx * w / gw
			sx1 := (cx + 1) * w / gw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var sum, n uint64
			for sy := sy0; sy < sy1 && sy < h; sy++ {
				for sx := sx0; sx < sx1 && sx < w; sx++ {
					if pointMasked(b.Min.X+sx, b.Min.Y+sy, masks) {
						continue
					}
					sum += luma16(img, b.Min.X+sx, b.Min.Y+sy) >> 8
					n++
				}
			}
			if n > 0 {
				cells[cy][cx] = sum / n
			}
		}
	}
	var hash uint64
	for cy := 0; cy < gh; cy++ {
		for cx := 0; cx < gw-1; cx++ {
			hash <<= 1
			if cells[cy][cx] > cells[cy][cx+1] {
				hash |= 1
			}
		}
	}
	return hash
}

// HashDistance returns the Hamming distance between two DHash values
// (0 = identical, 64 = every bit differs).
func HashDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}
