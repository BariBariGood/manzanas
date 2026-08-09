package actions

import (
	"context"
	"fmt"
	"time"
)

// Opt-in log correlation: payload {"capture_logs": true} on a HID or
// composite action collects the simulator's os_log lines emitted during
// the action window (start-of-action to completion, plus a small lead)
// and returns them in the result's "logs" field. The server journals
// the lines as a run artifact next to the action's journal entry, so an
// exported journal shows what the app logged at each step. Best-effort:
// a collection failure degrades to a "log_error" field, never a failed
// action. Simulator targets only (simctl spawn log); the device backend
// does not support it.

const (
	// logCaptureTimeout bounds the `log show` collection so a slow
	// logd cannot stall the action response.
	logCaptureTimeout = 15 * time.Second
	// logCaptureMaxBytes bounds the returned log text; the newest lines
	// win because the action's own effect logs last.
	logCaptureMaxBytes = 64 * 1024
	// logCaptureLead widens the window slightly before the action start
	// so lines racing the first input are not missed.
	logCaptureLead = time.Second
)

// logCaptureSpec is the parsed capture request for one action.
type logCaptureSpec struct {
	enabled bool
	process string // optional process-name filter
}

// logCaptureFromPayload reads the capture_logs / log_process fields.
// A log_process on its own implies capture_logs, so naming the process
// whose lines you want is enough (matches the CLI's --log-process).
func logCaptureFromPayload(p map[string]any) (logCaptureSpec, error) {
	spec := logCaptureSpec{}
	var err error
	if spec.enabled, err = boolFlag(p, "capture_logs", false); err != nil {
		return spec, err
	}
	if raw, ok := p["log_process"]; ok {
		s, ok := raw.(string)
		if !ok || s == "" {
			return spec, badRequest("payload field %q must be a non-empty process name (e.g. the app's executable name)", "log_process")
		}
		spec.process = s
		spec.enabled = true
	}
	return spec, nil
}

// captureLogs collects the simulator's os_log lines since start via
// `simctl spawn <udid> log show`, optionally filtered to one process.
func (b *AXeBackend) captureLogs(ctx context.Context, udid string, start time.Time, spec logCaptureSpec) (string, error) {
	lctx, cancel := context.WithTimeout(ctx, logCaptureTimeout)
	defer cancel()
	args := []string{"spawn", udid, "log", "show", "--style", "compact",
		// Include the UTC offset: a zone-less timestamp would be parsed in
		// the simulator's local zone, which need not match the host's.
		"--start", start.Add(-logCaptureLead).Format("2006-01-02 15:04:05-0700")}
	if spec.process != "" {
		args = append(args, "--predicate", fmt.Sprintf("process == %q", spec.process))
	}
	out, err := b.simctl(lctx, args...)
	if err != nil {
		return "", err
	}
	return truncateLogTail(string(out), logCaptureMaxBytes), nil
}

// truncateLogTail keeps the newest max bytes of the log text, prefixing
// a truncation marker when lines were dropped.
func truncateLogTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	dropped := len(s) - max
	return fmt.Sprintf("...[%d bytes truncated]...\n%s", dropped, s[dropped:])
}
