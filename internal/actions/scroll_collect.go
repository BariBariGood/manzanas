package actions

import (
	"context"
	"strings"
)

// scroll_collect: harvest the visible text boxes of every screen while
// gesturing down a list, with dedup so text that stays on screen across
// gestures (headers, sticky chrome, partial overlap) appears once — in
// the first screen it was seen on. The loop shares scroll_until's
// gesture → settle → moved-check machinery and stops at end-of-list or
// max_scrolls, so one call replaces an agent round-trip per screen.

func elemScrollCollect(ctx context.Context, d listDriver, udid string, p map[string]any) (map[string]any, error) {
	lp, err := listScrollParamsFromPayload(p)
	if err != nil {
		return nil, err
	}
	rect, err := collectRect(p)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	screens := []map[string]any{}
	collected := 0
	res, err := listScrollLoop(ctx, d, udid, lp, func(obs observation) bool {
		texts := harvestTexts(obs.nodes, obs.viewport, rect, seen)
		collected += len(texts)
		screens = append(screens, map[string]any{
			"index": len(screens),
			"texts": texts,
		})
		return false
	})
	if err != nil {
		return nil, err
	}
	out := res.fields()
	out["screens_texts"] = screens
	out["collected"] = collected
	return out, nil
}

// collectRect reads the optional rect payload field ({x, y, w, h}):
// only boxes whose centre falls inside it are harvested.
func collectRect(p map[string]any) (*Frame, error) {
	v, ok := p["rect"]
	if !ok {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, badRequest("payload field %q must be an object with numeric x, y, w, h", "rect")
	}
	var f Frame
	for _, k := range []string{"x", "y", "w", "h"} {
		n, err := toNum(m[k])
		if err != nil {
			return nil, badRequest("payload field %q must be an object with numeric x, y, w, h", "rect")
		}
		switch k {
		case "x":
			f.X = n
		case "y":
			f.Y = n
		case "w":
			f.W = n
		case "h":
			f.H = n
		}
	}
	if f.W <= 0 || f.H <= 0 {
		return nil, badRequest("payload field %q must have positive w and h", "rect")
	}
	return &f, nil
}

// harvestTexts flattens one screen's visible text runs into {text, x, y,
// w, h} boxes, skipping runs already seen (dedup key: the normalized
// run), runs whose centre is outside the viewport (a11y trees carry
// off-screen content), and, when rect is set, boxes whose centre is
// outside it. A run with no frame is only harvestable when no rect
// constrains the region.
func harvestTexts(nodes []*Node, viewport, rect *Frame, seen map[string]struct{}) []map[string]any {
	texts := []map[string]any{}
	var walk func(ns []*Node)
	walk = func(ns []*Node) {
		for _, n := range ns {
			if n == nil {
				continue
			}
			for _, s := range []string{n.Label, n.Value, n.Placeholder} {
				// minLen 1: the harvest reports every visible run, so
				// single-character entries (a "1", an "X") count — the
				// movement text set's OCR-noise minimum does not apply.
				key := normalizeRun(s, 1)
				if key == "" {
					continue
				}
				if _, dup := seen[key]; dup {
					continue
				}
				if n.Frame != nil && viewport != nil {
					cx, cy := n.Frame.Center()
					if !pointInViewport(cx, cy, viewport) {
						continue
					}
				}
				if rect != nil {
					if n.Frame == nil {
						continue
					}
					cx, cy := n.Frame.Center()
					if !pointInViewport(cx, cy, rect) {
						continue
					}
				}
				seen[key] = struct{}{}
				box := map[string]any{"text": strings.TrimSpace(s)}
				if n.Frame != nil {
					box["x"], box["y"] = n.Frame.X, n.Frame.Y
					box["w"], box["h"] = n.Frame.W, n.Frame.H
				}
				texts = append(texts, box)
			}
			walk(n.Children)
		}
	}
	walk(nodes)
	return texts
}

func handleScrollCollect(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	return elemScrollCollect(ctx, b, udid, p)
}

func handleDeviceScrollCollect(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	if _, err := b.wdaFor(udid); err != nil {
		return nil, err
	}
	return elemScrollCollect(ctx, b, udid, p)
}

func handleMirrorScrollCollect(ctx context.Context, b *DeviceBackend, udid string, p map[string]any) (map[string]any, error) {
	return withMirrorFidelity(elemScrollCollect(ctx, b, udid, p))
}
