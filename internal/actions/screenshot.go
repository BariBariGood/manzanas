package actions

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/BariBariGood/manzanas/internal/imgutil"
)

// handleScreenshot captures the target's screen. The capture is PNG; the
// optional payload fields format ("png" default | "jpeg"), quality (JPEG,
// 1-100) and max_dim (longer-edge pixel cap, downscaled server-side)
// shrink the response before it crosses the wire. AXe is preferred;
// `xcrun simctl io ... screenshot` is the fallback when AXe is absent.
func handleScreenshot(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	// inline is consumed by the server (which strips the base64 payload
	// from the wire response), but a malformed value is still rejected up
	// front.
	if _, err := boolFlag(p, "inline", true); err != nil {
		return nil, err
	}
	format, quality, maxDim, err := screenshotParams(p)
	if err != nil {
		return nil, err
	}
	img, backend, err := b.capturePNG(ctx, udid)
	if err != nil {
		return nil, err
	}
	if format != "png" || maxDim > 0 {
		img, err = imgutil.Transcode(img, format, quality, maxDim)
		if err != nil {
			return nil, internal("screenshot transform failed: %v", err)
		}
	}
	sum := sha256.Sum256(img)
	res := map[string]any{
		"format":  format,
		"bytes":   len(img),
		"sha256":  hex.EncodeToString(sum[:]),
		"backend": backend,
	}
	// The image is always returned inline: the server journals it as a
	// content-addressed artifact and honors the caller's inline:false by
	// stripping the base64 payload from the wire response.
	res[format+"_base64"] = base64.StdEncoding.EncodeToString(img)
	return res, nil
}

// capturePNG captures the target's screen as native-resolution PNG bytes,
// reporting which tool produced it ("axe" or "simctl").
func (b *AXeBackend) capturePNG(ctx context.Context, udid string) ([]byte, string, error) {
	dir, err := os.MkdirTemp(b.tempDir, "manzanasd-shot-")
	if err != nil {
		return nil, "", internal("cannot create temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "screen.png")

	backend := "axe"
	if b.AXeAvailable() {
		_, err = b.axe(ctx, udid, "screenshot", "--output", path)
	} else {
		backend = "simctl"
		_, err = b.simctl(ctx, "io", udid, "screenshot", path)
	}
	if err != nil {
		return nil, "", err
	}
	img, err := os.ReadFile(path)
	if err != nil {
		return nil, "", internal("screenshot file unreadable: %v", err)
	}
	return img, backend, nil
}

// screenshotParams reads the optional format/quality/max_dim payload
// fields, validating types and ranges.
func screenshotParams(p map[string]any) (format string, quality, maxDim int, err error) {
	format = "png"
	if v, ok := p["format"]; ok {
		s, ok := v.(string)
		if !ok || (s != "png" && s != "jpeg") {
			return "", 0, 0, badRequest("payload field %q must be \"png\" or \"jpeg\"", "format")
		}
		format = s
	}
	if v, ok := p["quality"]; ok {
		n, e := toNum(v)
		if e != nil || n < 1 || n > 100 {
			return "", 0, 0, badRequest("payload field %q must be a number in 1..100", "quality")
		}
		if format != "jpeg" {
			return "", 0, 0, badRequest("payload field %q requires format \"jpeg\"", "quality")
		}
		quality = int(n)
	}
	if v, ok := p["max_dim"]; ok {
		n, e := toNum(v)
		if e != nil || n < 1 {
			return "", 0, 0, badRequest("payload field %q must be a positive number of pixels", "max_dim")
		}
		maxDim = int(n)
	}
	return format, quality, maxDim, nil
}
