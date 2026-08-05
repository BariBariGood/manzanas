package actions

import (
	"context"
)

// handlePasteboardSet copies text onto the simulator's pasteboard via
// `simctl pbcopy` (content is passed on stdin, so arbitrary text is safe).
func handlePasteboardSet(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	text, ok := p["text"].(string)
	if !ok {
		return nil, badRequest("pasteboard_set requires a string payload field %q", "text")
	}
	if _, err := b.simctlInput(ctx, []byte(text), "pbcopy", udid); err != nil {
		return nil, err
	}
	return map[string]any{"copied_runes": len([]rune(text))}, nil
}

// handlePasteboardGet reads the simulator's pasteboard via `simctl pbpaste`.
func handlePasteboardGet(ctx context.Context, b *AXeBackend, udid string, p map[string]any) (map[string]any, error) {
	out, err := b.simctl(ctx, "pbpaste", udid)
	if err != nil {
		return nil, err
	}
	return map[string]any{"text": string(out)}, nil
}

// simctlInput runs `xcrun simctl` with stdin content, for subcommands like
// pbcopy that consume standard input.
func (b *AXeBackend) simctlInput(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	ir, ok := b.runner.(InputRunner)
	if !ok {
		return nil, unavailable("this build's command runner does not support stdin (needed for simctl %s)", args[0])
	}
	out, stderr, err := ir.RunInput(ctx, stdin, b.xcrun, append([]string{"simctl"}, args...)...)
	if err != nil {
		return nil, internal("simctl %s failed: %v: %s", args[0], err, trim(stderr))
	}
	return out, nil
}
