package state

import (
	"context"
	"fmt"
)

// localeFixture sets the simulator's language and locale by writing the
// global preferences domain via `simctl spawn <udid> defaults`.
// Payload: {"language": "zh-Hans" | ["zh-Hans", "en"], "locale": "zh_CN"}
// (at least one required). Requires a booted target; already-running apps
// pick the change up on next launch (a reboot applies it system-wide).
type localeFixture struct{}

func (localeFixture) Name() string { return "locale" }

func (localeFixture) Apply(ctx context.Context, run Runner, udid string, payload map[string]any) error {
	langs := payloadLanguages(payload["language"])
	locale, _ := payload["locale"].(string)
	if len(langs) == 0 && locale == "" {
		return fmt.Errorf("%w: locale payload needs \"language\" and/or \"locale\"", ErrBadFixture)
	}
	for _, l := range append(langs, locale) {
		if l == "" {
			continue
		}
		if err := flagSafe(l); err != nil {
			return fmt.Errorf("%w: %v", ErrBadFixture, err)
		}
	}
	if len(langs) > 0 {
		args := []string{"spawn", udid, "defaults", "write", ".GlobalPreferences", "AppleLanguages", "-array"}
		args = append(args, langs...)
		if _, err := run.Simctl(ctx, args...); err != nil {
			return err
		}
	}
	if locale != "" {
		if _, err := run.Simctl(ctx, "spawn", udid, "defaults", "write", ".GlobalPreferences", "AppleLocale", "-string", locale); err != nil {
			return err
		}
	}
	return nil
}

func payloadLanguages(v any) []string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
