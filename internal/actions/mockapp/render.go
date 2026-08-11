package mockapp

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"strings"
)

// Rendering palette: flat, high-contrast fills so screenshots (and the
// annotated audit output drawn over them) stay legible.
var (
	renderBG        = color.RGBA{245, 245, 247, 255}
	renderText      = color.RGBA{20, 20, 24, 255}
	renderFieldBG   = color.RGBA{255, 255, 255, 255}
	renderFieldEdge = color.RGBA{190, 190, 196, 255}
	renderButton    = color.RGBA{0, 122, 255, 255}
	renderSwitchOn  = color.RGBA{52, 199, 89, 255}
	renderSwitchOff = color.RGBA{174, 174, 178, 255}
	renderKeyboard  = color.RGBA{209, 212, 217, 255}
	renderKey       = color.RGBA{252, 252, 254, 255}
	renderWhite     = color.RGBA{255, 255, 255, 255}
	renderMuted     = color.RGBA{140, 140, 148, 255}
)

// RenderPNG renders the current screen as a PNG at point resolution.
// The pixels are drawn from the same element list describe-ui reports,
// so screenshots always match the observed tree.
func (a *App) RenderPNG() ([]byte, error) {
	img := a.render()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// RenderJPEG renders the current screen as a JPEG stream frame.
func (a *App) RenderJPEG() ([]byte, error) {
	img := a.render()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 70}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (a *App) render() *image.RGBA {
	a.mu.Lock()
	defer a.mu.Unlock()
	img := image.NewRGBA(image.Rect(0, 0, ScreenW, ScreenH))
	fillRect(img, 0, 0, ScreenW, ScreenH, renderBG)
	for _, e := range a.elements() {
		x, y, w, h := a.screenFrame(e)
		xi, yi, wi, hi := int(x), int(y), int(w), int(h)
		switch e.Role {
		case "StaticText":
			drawText(img, e.Label, xi, yi+hi/2-7, 3, renderText)
		case "TextField", "SecureTextField":
			fillRect(img, xi, yi, wi, hi, renderFieldBG)
			strokeRect(img, xi, yi, wi, hi, 2, renderFieldEdge)
			if e.Value != "" {
				drawText(img, e.Value, xi+10, yi+hi/2-7, 3, renderText)
			} else {
				drawText(img, e.Placeholder, xi+10, yi+hi/2-7, 3, renderMuted)
			}
		case "Button":
			fillRect(img, xi, yi, wi, hi, renderButton)
			drawTextCentered(img, e.Label, xi, yi, wi, hi, 3, renderWhite)
		case "Switch":
			c := renderSwitchOff
			knobX := xi + 4
			if e.Value == "1" {
				c = renderSwitchOn
				knobX = xi + wi - hi + 4
			}
			fillRect(img, xi, yi, wi, hi, c)
			fillRect(img, knobX, yi+4, hi-8, hi-8, renderWhite)
			drawText(img, e.Label, xi+wi+12, yi+hi/2-7, 3, renderText)
		case "Keyboard":
			fillRect(img, xi, yi, wi, hi, renderKeyboard)
			for row := 0; row < 4; row++ {
				for col := 0; col < 10; col++ {
					fillRect(img, xi+8+col*38, yi+16+row*56, 30, 42, renderKey)
				}
			}
		}
	}
	return img
}

func fillRect(dst *image.RGBA, x, y, w, h int, c color.RGBA) {
	r := image.Rect(x, y, x+w, y+h).Intersect(dst.Bounds())
	if r.Empty() {
		return
	}
	draw.Draw(dst, r, &image.Uniform{c}, image.Point{}, draw.Src)
}

func strokeRect(dst *image.RGBA, x, y, w, h, stroke int, c color.RGBA) {
	fillRect(dst, x, y, w, stroke, c)
	fillRect(dst, x, y+h-stroke, w, stroke, c)
	fillRect(dst, x, y, stroke, h, c)
	fillRect(dst, x+w-stroke, y, stroke, h, c)
}

// drawText renders a string with the 3x5 bitmap font at the given scale.
func drawText(dst *image.RGBA, s string, x, y, scale int, c color.RGBA) {
	cx := x
	for _, r := range strings.ToUpper(s) {
		drawGlyph(dst, r, cx, y, scale, c)
		cx += 4 * scale
	}
}

func drawTextCentered(dst *image.RGBA, s string, x, y, w, h, scale int, c color.RGBA) {
	tw := len([]rune(s))*4*scale - scale
	drawText(dst, s, x+(w-tw)/2, y+(h-5*scale)/2, scale, c)
}

func drawGlyph(dst *image.RGBA, r rune, x, y, scale int, c color.RGBA) {
	rows, ok := glyphs[r]
	if !ok {
		rows = [5]uint8{0b111, 0b111, 0b111, 0b111, 0b111}
	}
	for ry, bits := range rows {
		for rx := 0; rx < 3; rx++ {
			if bits&(1<<(2-rx)) == 0 {
				continue
			}
			fillRect(dst, x+rx*scale, y+ry*scale, scale, scale, c)
		}
	}
}
