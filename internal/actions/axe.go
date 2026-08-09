package actions

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// axHashTimeout bounds each opt-in a11y hash around a HID action so the
// evidence describe-ui (which retries with backoff on a slow bridge)
// can't dominate the action's latency.
const axHashTimeout = 5 * time.Second

// handlerFunc executes one action kind against a target UDID and returns
// the result payload.
type handlerFunc func(ctx context.Context, b *AXeBackend, udid string, payload map[string]any) (map[string]any, error)

// AXeBackend implements Backend by shelling out to the AXe CLI
// (github.com/cameroncooke/AXe) for HID and a11y, and to `xcrun simctl`
// for app lifecycle. Both are abstracted behind Runner so an
// FBSimulatorControl-native or simctl-only backend can be swapped later.
type AXeBackend struct {
	runner   Runner
	axePath  string // "" when AXe is not available
	xcrun    string
	handlers map[string]handlerFunc
	tempDir  string
	// warmObserve, when set, runs a single a11y poll through a resident
	// warm helper instead of spawning the AXe CLI; a transport failure
	// falls back to the cold poll.
	warmObserve func(ctx context.Context, udid string) (observation, error)
	// warmHID, when set, sends one HID op through a resident warm helper;
	// a transport failure before delivery falls back to the cold CLI.
	warmHID func(ctx context.Context, udid, op string, args map[string]any) error
	// axeDetected marks axePath as explicitly set, skipping auto-detection.
	axeDetected bool
}

// Option configures an AXeBackend.
type Option func(*AXeBackend)

// WithRunner swaps the command runner (tests).
func WithRunner(r Runner) Option { return func(b *AXeBackend) { b.runner = r } }

// WithAXePath forces the AXe binary path ("" marks AXe unavailable).
func WithAXePath(p string) Option { return func(b *AXeBackend) { b.axePath = p; b.axeDetected = true } }

// WithTempDir sets the directory used for screenshot temp files.
func WithTempDir(d string) Option { return func(b *AXeBackend) { b.tempDir = d } }

// NewAXe builds the backend, auto-detecting the AXe binary (~/bin/axe or
// $PATH) unless overridden. A missing AXe does not fail construction; HID
// and observe actions return an "unavailable" error until it is installed.
func NewAXe(opts ...Option) *AXeBackend {
	b := &AXeBackend{
		runner:  ExecRunner{},
		xcrun:   "xcrun",
		tempDir: os.TempDir(),
	}
	for _, o := range opts {
		o(b)
	}
	if !b.axeDetected {
		b.axePath = detectAXe()
	}
	b.handlers = map[string]handlerFunc{
		"tap":               handleTap,
		"swipe":             handleSwipe,
		"type":              handleType,
		"button":            handleButton,
		"key":               handleKey,
		"key_sequence":      handleKeySequence,
		"tap_element":       handleTapElement,
		"type_into_element": handleTypeIntoElement,
		"scroll_to_element": handleScrollToElement,
		"observe":           handleObserve,
		"wait_for_element":  handleWaitForElement,
		"wait_tree_stable":  handleWaitTreeStable,
		"pasteboard_get":    handlePasteboardGet,
		"pasteboard_set":    handlePasteboardSet,
		"screenshot":        handleScreenshot,
		"audit":             handleAudit,
		"install_app":       handleInstallApp,
		"launch_app":        handleLaunchApp,
		"terminate_app":     handleTerminateApp,
	}
	return b
}

// SetWarmObserver installs an accelerated single-poll observer used by
// the wait_* actions (and any other per-poll a11y read); a poll whose
// warm attempt hits a transport failure runs cold instead.
func (b *AXeBackend) SetWarmObserver(fn func(ctx context.Context, udid string) (observation, error)) {
	b.warmObserve = fn
}

// SetWarmHID installs an accelerated single-op HID dispatcher used by
// the composite element actions; an op whose warm attempt hits a
// transport failure before delivery runs cold instead.
func (b *AXeBackend) SetWarmHID(fn func(ctx context.Context, udid, op string, args map[string]any) error) {
	b.warmHID = fn
}

// dispatchHID sends one HID input, preferring the resident warm helper.
// A transport failure before delivery — or a helper that doesn't
// implement the op (errColdOnly) — re-runs the input on the cold CLI; a
// failure after possible delivery surfaces (a cold retry could apply
// the input twice).
func (b *AXeBackend) dispatchHID(ctx context.Context, udid, op string, args map[string]any, argv ...string) error {
	if b.warmHID != nil {
		err := b.warmHID(ctx, udid, op, args)
		if !errors.Is(err, errColdOnly) {
			var te *TransportError
			if !errors.As(err, &te) {
				return err
			}
			if te.Delivered {
				return unavailable("warm helper failed mid-%s; the input may or may not have been delivered", op)
			}
		}
	}
	_, err := b.axe(ctx, udid, argv...)
	return err
}

// AXeAvailable reports whether an AXe binary was found.
func (b *AXeBackend) AXeAvailable() bool { return b.axePath != "" }

// AXePath returns the resolved AXe binary path ("" when unavailable).
func (b *AXeBackend) AXePath() string { return b.axePath }

// detectAXe looks for the AXe binary at ~/bin/axe, then on $PATH.
func detectAXe() string {
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, "bin", "axe")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("axe"); err == nil {
		return p
	}
	return ""
}

// hidKinds are the UI-mutating actions that support opt-in before/after
// a11y tree hashing via payload {"ax_hashes": true}.
var hidKinds = map[string]bool{
	"tap": true, "swipe": true, "type": true, "button": true,
	"key": true, "key_sequence": true,
	"tap_element": true, "type_into_element": true, "scroll_to_element": true,
}

// logKinds are the actions that support opt-in os_log capture around the
// action window via payload {"capture_logs": true} (see logs.go): the
// HID/composite kinds plus app lifecycle, whose launch/termination logs
// are exactly what a debugging agent wants correlated.
var logKinds = map[string]bool{
	"tap": true, "swipe": true, "type": true, "button": true,
	"key": true, "key_sequence": true,
	"tap_element": true, "type_into_element": true, "scroll_to_element": true,
	"launch_app": true, "terminate_app": true,
}

// Dispatch implements Backend.
func (b *AXeBackend) Dispatch(ctx context.Context, udid string, req proto.ActionRequest) (proto.ActionResult, error) {
	h, ok := b.handlers[req.Kind]
	if !ok {
		return proto.ActionResult{}, badRequest("unknown action kind %q", req.Kind)
	}
	// Opt-in a11y evidence around HID actions: hash the tree before and
	// after so the journal entry carries ax_before/ax_after. Best-effort
	// and time-bounded (axHashTimeout each) — a slow or warming a11y
	// bridge degrades to a missing hash, never a stalled or failed action.
	wantAX := false
	if hidKinds[req.Kind] {
		v, err := boolFlag(req.Payload, "ax_hashes", false)
		if err != nil {
			return proto.ActionResult{}, err
		}
		wantAX = v && b.axePath != ""
	}
	var logSpec logCaptureSpec
	if logKinds[req.Kind] {
		var err error
		if logSpec, err = logCaptureFromPayload(req.Payload); err != nil {
			return proto.ActionResult{}, err
		}
	}
	var axBefore string
	if wantAX {
		axBefore = b.hashTree(ctx, udid)
	}
	start := time.Now()
	res, err := h(ctx, b, udid, req.Payload)
	if err != nil {
		return proto.ActionResult{}, err
	}
	if wantAX {
		if res == nil {
			res = map[string]any{}
		}
		if axBefore != "" {
			res["ax_before"] = axBefore
		}
		if h := b.hashTree(ctx, udid); h != "" {
			res["ax_after"] = h
		}
	}
	if logSpec.enabled {
		if res == nil {
			res = map[string]any{}
		}
		// Best-effort: the action already succeeded; a log collection
		// failure degrades to log_error, never a failed action.
		if logs, lerr := b.captureLogs(ctx, udid, start, logSpec); lerr != nil {
			res["log_error"] = lerr.Error()
		} else {
			res["logs"] = logs
		}
	}
	return proto.ActionResult{OK: true, Result: res}, nil
}

// hashTree returns the current a11y tree hash, or "" on failure/timeout.
func (b *AXeBackend) hashTree(ctx context.Context, udid string) string {
	hctx, cancel := context.WithTimeout(ctx, axHashTimeout)
	defer cancel()
	_, nodes, err := b.observeTree(hctx, udid)
	if err != nil {
		return ""
	}
	return TreeHash(nodes)
}

// axe runs the AXe binary with --udid appended, returning stdout.
func (b *AXeBackend) axe(ctx context.Context, udid string, args ...string) ([]byte, error) {
	if b.axePath == "" {
		return nil, unavailable("axe binary not found on this host; install AXe (github.com/cameroncooke/AXe) at ~/bin/axe")
	}
	args = append(args, "--udid", udid)
	out, stderr, err := b.runner.Run(ctx, b.axePath, args...)
	if err != nil {
		if isNotBooted(stderr) {
			return nil, targetNotBooted(udid)
		}
		return nil, internal("axe %s failed: %v: %s", args[0], err, trim(stderr))
	}
	return out, nil
}

// simctl runs `xcrun simctl` with the given args, returning stdout.
func (b *AXeBackend) simctl(ctx context.Context, args ...string) ([]byte, error) {
	out, stderr, err := b.runner.Run(ctx, b.xcrun, append([]string{"simctl"}, args...)...)
	if err != nil {
		if isNotBooted(stderr) {
			return nil, targetNotBooted("")
		}
		return nil, internal("simctl %s failed: %v: %s", args[0], err, trim(stderr))
	}
	return out, nil
}

// isNotBooted recognizes the tool-specific ways AXe/FBSimulatorControl and
// simctl complain about a shutdown target, so a leased-but-unbooted
// simulator surfaces a stable protocol error instead of raw stderr.
func isNotBooted(stderr []byte) bool {
	s := strings.ToLower(string(stderr))
	return strings.Contains(s, "is not booted") ||
		strings.Contains(s, "current state: shutdown") ||
		strings.Contains(s, "state: fbiostargetstatestring(_rawvalue: shutdown)") ||
		strings.Contains(s, "unable to lookup in current state: shutdown")
}

func trim(b []byte) string {
	const max = 512
	s := string(b)
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
