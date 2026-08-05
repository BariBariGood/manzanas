package journal

import (
	"context"
	"fmt"
	"strings"
)

// ExportMarkdown renders a PR-comment-ready evidence summary for a run: run
// metadata, an action table, and the artifacts each step produced.
func (s *FileStore) ExportMarkdown(ctx context.Context, runID string) (string, error) {
	entries, err := s.Read(ctx, runID, 0, 0)
	if err != nil {
		return "", err
	}
	meta, err := s.ReadMeta(runID)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## manzanasd run journal — `%s`\n\n", runID)
	fmt.Fprintf(&b, "| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Format | `%s` |\n", mdEscape(orDash(meta.FormatVersion)))
	fmt.Fprintf(&b, "| Agent | %s |\n", mdEscape(orDash(meta.AgentID)))
	fmt.Fprintf(&b, "| Purpose | %s |\n", mdEscape(orDash(meta.Purpose)))
	fmt.Fprintf(&b, "| Target | %s (`%s`) |\n", mdEscape(orDash(meta.TargetName)), mdEscape(orDash(meta.TargetUDID)))
	fmt.Fprintf(&b, "| Runtime | %s |\n", mdEscape(orDash(meta.Runtime)))
	fmt.Fprintf(&b, "| Device type | %s |\n", mdEscape(orDash(meta.DeviceType)))
	if !meta.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "| Started | %s |\n", meta.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	fmt.Fprintf(&b, "| Entries | %d |\n", len(entries))

	b.WriteString("\n### Actions\n\n")
	b.WriteString("| # | Time (UTC) | Kind | Action | Status | Detail |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	var artifacts []string
	for _, e := range entries {
		p := e.Payload
		ts := payloadTime(p)
		action := payloadString(p, "action")
		status := payloadString(p, "status")
		// summarizeParams escapes its own pieces (it adds backticks that
		// must survive), so only the raw error string is escaped here.
		detail := mdEscape(payloadString(p, "error"))
		if detail == "" {
			detail = summarizeParams(p)
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s | %s |\n",
			e.Ref.Seq, ts, mdEscape(e.Kind), mdEscape(action), mdEscape(status), detail)
		artifacts = append(artifacts, artifactNames(p, e.Ref.Seq)...)
	}

	if len(artifacts) > 0 {
		b.WriteString("\n### Artifacts\n\n")
		for _, a := range artifacts {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	return b.String(), nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func payloadString(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	if v, ok := p[key].(string); ok {
		return v
	}
	return ""
}

func payloadTime(p map[string]any) string {
	ts := payloadString(p, "ts")
	// Trim RFC 3339 to seconds for table readability.
	if i := strings.IndexByte(ts, '.'); i > 0 {
		ts = ts[:i] + "Z"
	}
	return strings.TrimSuffix(strings.Replace(ts, "T", " ", 1), "Z")
}

// summarizeParams renders a short inline `k=v` list from the entry's params.
func summarizeParams(p map[string]any) string {
	params, ok := p["params"].(map[string]any)
	if !ok || len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("`%s`", codeEscape(fmt.Sprintf("%s=%v", k, params[k]))))
	}
	s := strings.Join(parts, " ")
	if len(s) > 120 {
		// Drop whole parts so the inline code spans stay balanced and no
		// multi-byte rune is cut in half.
		for len(parts) > 0 && len(s) > 120 {
			parts = parts[:len(parts)-1]
			s = strings.Join(parts, " ")
		}
		s = strings.TrimSpace(s + " `…`")
	}
	return s
}

// artifactNames lists an entry's artifact refs as markdown bullets.
func artifactNames(p map[string]any, seq int64) []string {
	raw, ok := p["artifacts"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, a := range raw {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		path, _ := m["path"].(string)
		sha, _ := m["sha256"].(string)
		if len(sha) > 12 {
			sha = sha[:12]
		}
		out = append(out, fmt.Sprintf("`%s` (step %d, sha256 `%s…`%s)",
			path, seq, sha, recordingDetail(p)))
	}
	return out
}

// recordingDetail summarizes a recording segment's stats (duration, size,
// stop reason) for its artifact bullet; empty for non-recording entries.
func recordingDetail(p map[string]any) string {
	params, ok := p["params"].(map[string]any)
	if !ok {
		return ""
	}
	dur, ok := params["duration_s"].(float64)
	if !ok {
		return ""
	}
	bytes, _ := params["bytes"].(float64)
	detail := fmt.Sprintf(", %.1fs %.1f MiB video", dur, bytes/(1<<20))
	if reason, _ := params["reason"].(string); reason != "" {
		detail += ", " + mdEscape(reason)
	}
	return detail
}

// codeEscape neutralizes what can break out of an inline code span in a
// table cell: the closing backtick, the cell separator, and newlines.
// Other markdown is inert inside code spans.
func codeEscape(s string) string {
	return strings.NewReplacer("`", "'", "|", "\\|", "\n", " ").Replace(s)
}

// mdEscape neutralizes markdown/HTML in untrusted values rendered into the
// evidence tables: table separators, newlines, inline-code breakouts, HTML
// tags, and link/image syntax.
func mdEscape(s string) string {
	return strings.NewReplacer(
		"|", "\\|",
		"\n", " ",
		"`", "\\`",
		"<", "\\<",
		"[", "\\[",
		"!", "\\!",
	).Replace(s)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
