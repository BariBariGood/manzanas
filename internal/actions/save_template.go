package actions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg" // WDA builds may answer JPEG captures
	"image/png"

	"github.com/BariBariGood/manzanas/internal/templates"
)

// save_template captures a FRESH frame through the backend's screenshot
// path, crops the payload rect out of it, and stores the crop in the
// template library (internal/templates) keyed by
// bundle_id/name@<backend>/<img_w>. Matching (tap_template) is a separate
// action; this is the storage half.

// defaultTemplateBundle groups templates saved without a bundle_id.
const defaultTemplateBundle = "_global"

// saveTemplateSpec is the validated save_template payload.
type saveTemplateSpec struct {
	name      string
	bundleID  string
	rect      templates.Rect
	sourceRun string
}

// saveTemplateSpecFromPayload validates {name, rect{x,y,w,h}, bundle_id?,
// source_run?}.
func saveTemplateSpecFromPayload(p map[string]any) (saveTemplateSpec, error) {
	spec := saveTemplateSpec{bundleID: defaultTemplateBundle}
	name, _ := p["name"].(string)
	if name == "" {
		return spec, badRequest("save_template requires a non-empty string payload field %q", "name")
	}
	if err := templates.ValidateName(name); err != nil {
		return spec, badRequest("%v", err)
	}
	spec.name = name
	if v, ok := p["bundle_id"]; ok {
		s, _ := v.(string)
		if s == "" {
			return spec, badRequest("payload field %q must be a non-empty string when present", "bundle_id")
		}
		if err := templates.ValidateBundleID(s); err != nil {
			return spec, badRequest("%v", err)
		}
		spec.bundleID = s
	}
	if v, ok := p["source_run"]; ok {
		s, _ := v.(string)
		spec.sourceRun = s
	}
	rectVal, ok := p["rect"].(map[string]any)
	if !ok {
		return spec, badRequest("save_template requires payload field %q ({x,y,w,h} pixels in the capture image)", "rect")
	}
	var vals [4]int
	for i, k := range []string{"x", "y", "w", "h"} {
		n, err := toNum(rectVal[k])
		if err != nil || n != float64(int(n)) {
			return spec, badRequest("rect field %q must be an integer number of pixels", k)
		}
		vals[i] = int(n)
	}
	spec.rect = templates.Rect{X: vals[0], Y: vals[1], W: vals[2], H: vals[3]}
	if spec.rect.X < 0 || spec.rect.Y < 0 {
		return spec, badRequest("rect x/y must be >= 0")
	}
	if spec.rect.W <= 0 || spec.rect.H <= 0 {
		return spec, badRequest("rect w/h must be > 0")
	}
	// Cap each field well below any plausible capture size so the x+w /
	// y+h fit check in cropPNG can never overflow (and a huge rect can
	// never reach image.NewRGBA, which panics on absurd dimensions).
	const maxRectPx = 1 << 20
	if spec.rect.X > maxRectPx || spec.rect.Y > maxRectPx ||
		spec.rect.W > maxRectPx || spec.rect.H > maxRectPx {
		return spec, badRequest("rect fields must be at most %d pixels", maxRectPx)
	}
	return spec, nil
}

// cropPNG decodes a capture, crops rect out of it, and re-encodes the
// crop as PNG, reporting the full capture size.
func cropPNG(capture []byte, rect templates.Rect) (crop []byte, imgW, imgH int, err error) {
	src, _, err := image.Decode(bytes.NewReader(capture))
	if err != nil {
		return nil, 0, 0, internal("save_template: decode capture: %v", err)
	}
	b := src.Bounds()
	imgW, imgH = b.Dx(), b.Dy()
	if rect.X+rect.W > imgW || rect.Y+rect.H > imgH {
		return nil, 0, 0, badRequest(
			"rect {x:%d,y:%d,w:%d,h:%d} exceeds the capture size %dx%d",
			rect.X, rect.Y, rect.W, rect.H, imgW, imgH)
	}
	dst := image.NewRGBA(image.Rect(0, 0, rect.W, rect.H))
	draw.Draw(dst, dst.Bounds(), src, image.Pt(b.Min.X+rect.X, b.Min.Y+rect.Y), draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, 0, 0, internal("save_template: encode crop: %v", err)
	}
	return buf.Bytes(), imgW, imgH, nil
}

// saveTemplate crops a capture and stores the result, building the shared
// result payload. backend names the capture tool and keys the device
// class ("<backend>/<img_w>").
func saveTemplate(store *templates.Store, spec saveTemplateSpec, capture []byte, backend string) (map[string]any, error) {
	crop, imgW, imgH, err := cropPNG(capture, spec.rect)
	if err != nil {
		return nil, err
	}
	class := fmt.Sprintf("%s/%d", backend, imgW)
	entry, err := store.Put(spec.bundleID, spec.name, crop, templates.Meta{
		DeviceClass: class,
		WindowPx:    [2]int{imgW, imgH},
		CropRect:    spec.rect,
		SourceRun:   spec.sourceRun,
	})
	if err != nil {
		return nil, internal("save_template: store: %v", err)
	}
	sum := sha256.Sum256(crop)
	return map[string]any{
		"name":         entry.Name,
		"bundle_id":    entry.BundleID,
		"device_class": class,
		"window_px":    []int{imgW, imgH},
		"rect": map[string]any{
			"x": spec.rect.X, "y": spec.rect.Y, "w": spec.rect.W, "h": spec.rect.H,
		},
		"bytes":   len(crop),
		"sha256":  hex.EncodeToString(sum[:]),
		"backend": backend,
	}, nil
}

// requireTemplateStore rejects a save on a daemon without a template
// library BEFORE any capture is taken: a capture is real work (and on
// the mirror backend drives the exclusive GUI helper).
func requireTemplateStore(store *templates.Store) error {
	if store == nil {
		return unavailable("the template library is not configured on this daemon (--templates-dir)")
	}
	return nil
}

// handleSaveTemplate is the simulator save_template: a fresh capturePNG
// (AXe or simctl), cropped and stored.
func handleSaveTemplate(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	if err := requireTemplateStore(b.templates); err != nil {
		return nil, err
	}
	spec, err := saveTemplateSpecFromPayload(p)
	if err != nil {
		return nil, err
	}
	capture, backend, err := b.capturePNG(ctx, udid)
	if err != nil {
		return nil, err
	}
	return saveTemplate(b.templates, spec, capture, backend)
}

// handleDeviceSaveTemplate is the WDA-device save_template: a fresh WDA
// screenshot, cropped and stored.
func handleDeviceSaveTemplate(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	if err := requireTemplateStore(b.templates); err != nil {
		return nil, err
	}
	spec, err := saveTemplateSpecFromPayload(p)
	if err != nil {
		return nil, err
	}
	c, err := b.wdaFor(udid)
	if err != nil {
		return nil, err
	}
	capture, err := c.Screenshot(ctx)
	if err != nil {
		return nil, b.wdaFail(udid, "save_template", err)
	}
	return saveTemplate(b.templates, spec, capture, "wda")
}

// handleMirrorSaveTemplate is the mirror-backed save_template: a fresh
// mirrord window capture, cropped and stored.
func handleMirrorSaveTemplate(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	if err := requireTemplateStore(b.templates); err != nil {
		return nil, err
	}
	spec, err := saveTemplateSpecFromPayload(p)
	if err != nil {
		return nil, err
	}
	c, err := b.mirrorClient(udid)
	if err != nil {
		return nil, err
	}
	scr, err := c.Screenshot(ctx)
	if err != nil {
		return nil, mirrorFail("save_template", err)
	}
	return saveTemplate(b.templates, spec, scr.PNG, "mirror")
}
