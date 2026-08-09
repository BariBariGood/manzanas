package imgutil

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// Box is one annotation rectangle in pixel coordinates, with an optional
// short label rendered at its top-left corner.
type Box struct {
	X, Y, W, H int
	Label      string
}

// annotationRed is the box/label color: opaque red, readable on both
// light and dark screens.
var annotationRed = color.RGBA{R: 220, G: 30, B: 30, A: 255}

// Annotate decodes src (PNG or JPEG), draws a red rectangle for every box
// with its label on a red tag at the top-left corner, and re-encodes the
// result as PNG. Boxes are clamped to the image bounds; an empty box list
// still returns a valid (re-encoded) image.
func Annotate(src []byte, boxes []Box) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	b := img.Bounds()
	dst := image.NewRGBA(b)
	draw.Draw(dst, b, img, b.Min, draw.Src)
	// Stroke and glyph size scale with the capture so annotations stay
	// legible on both point-resolution and Retina-pixel captures.
	long := b.Dx()
	if b.Dy() > long {
		long = b.Dy()
	}
	stroke := long / 400
	if stroke < 2 {
		stroke = 2
	}
	for _, box := range boxes {
		drawRect(dst, box, stroke)
		if box.Label != "" {
			drawTag(dst, box, stroke)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, fmt.Errorf("encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// drawRect strokes the box outline, clamped to the image bounds.
func drawRect(dst *image.RGBA, box Box, stroke int) {
	r := image.Rect(box.X, box.Y, box.X+box.W, box.Y+box.H).Intersect(dst.Bounds())
	if r.Empty() {
		return
	}
	fill := func(x0, y0, x1, y1 int) {
		draw.Draw(dst, image.Rect(x0, y0, x1, y1).Intersect(dst.Bounds()),
			&image.Uniform{annotationRed}, image.Point{}, draw.Src)
	}
	fill(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+stroke)
	fill(r.Min.X, r.Max.Y-stroke, r.Max.X, r.Max.Y)
	fill(r.Min.X, r.Min.Y, r.Min.X+stroke, r.Max.Y)
	fill(r.Max.X-stroke, r.Min.Y, r.Max.X, r.Max.Y)
}

// drawTag renders the label as white glyphs on a red tag anchored at the
// box's top-left corner (above the box when there is room, inside it
// otherwise).
func drawTag(dst *image.RGBA, box Box, stroke int) {
	scale := stroke
	glyphW, glyphH := 3*scale, 5*scale
	pad := scale
	w := pad*2 + len(box.Label)*(glyphW+scale) - scale
	h := glyphH + pad*2
	x, y := box.X, box.Y-h
	if y < dst.Bounds().Min.Y {
		y = box.Y
	}
	draw.Draw(dst, image.Rect(x, y, x+w, y+h).Intersect(dst.Bounds()),
		&image.Uniform{annotationRed}, image.Point{}, draw.Src)
	cx := x + pad
	for _, r := range box.Label {
		drawGlyph(dst, r, cx, y+pad, scale)
		cx += glyphW + scale
	}
}

// tagGlyphs is a minimal 3x5 bitmap font (rows top to bottom, 3 bits per
// row, MSB left) covering the characters annotation refs use.
var tagGlyphs = map[rune][5]uint8{
	'0': {0b111, 0b101, 0b101, 0b101, 0b111},
	'1': {0b010, 0b110, 0b010, 0b010, 0b111},
	'2': {0b111, 0b001, 0b111, 0b100, 0b111},
	'3': {0b111, 0b001, 0b111, 0b001, 0b111},
	'4': {0b101, 0b101, 0b111, 0b001, 0b001},
	'5': {0b111, 0b100, 0b111, 0b001, 0b111},
	'6': {0b111, 0b100, 0b111, 0b101, 0b111},
	'7': {0b111, 0b001, 0b010, 0b010, 0b010},
	'8': {0b111, 0b101, 0b111, 0b101, 0b111},
	'9': {0b111, 0b101, 0b111, 0b001, 0b111},
	'F': {0b111, 0b100, 0b111, 0b100, 0b100},
}

// drawGlyph paints one scaled font glyph in white; unknown runes render
// as a solid block so a bad label is still visible.
func drawGlyph(dst *image.RGBA, r rune, x, y, scale int) {
	rows, ok := tagGlyphs[r]
	if !ok {
		rows = [5]uint8{0b111, 0b111, 0b111, 0b111, 0b111}
	}
	white := &image.Uniform{color.RGBA{255, 255, 255, 255}}
	for ry, bits := range rows {
		for rx := 0; rx < 3; rx++ {
			if bits&(1<<(2-rx)) == 0 {
				continue
			}
			cell := image.Rect(x+rx*scale, y+ry*scale, x+(rx+1)*scale, y+(ry+1)*scale)
			draw.Draw(dst, cell.Intersect(dst.Bounds()), white, image.Point{}, draw.Src)
		}
	}
}
