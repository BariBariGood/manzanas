package mockapp

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/BariBariGood/manzanas/internal/actions"
)

// AXeBin is the pseudo AXe binary path the mock backend is constructed
// with; it never executes, it only routes Runner calls here.
const AXeBin = "mock-axe"

// BootedFunc reports whether a target is booted, so the mock backend
// refuses actions against shutdown targets exactly like real AXe/simctl.
type BootedFunc func(ctx context.Context, udid string) bool

// Runner implements actions.Runner (and actions.InputRunner) against the
// synthetic app store instead of shelling out, emulating the AXe and
// simctl argv surfaces the AXeBackend produces. Wired via
// actions.WithRunner, it reuses every real handler — observe compaction,
// predicate matching, wait loops, audit checks — over the synthetic tree.
type Runner struct {
	store  *Store
	booted BootedFunc
}

// RunnerOption configures a Runner.
type RunnerOption func(*Runner)

// WithBooted installs the booted check ("not booted" errors when false).
func WithBooted(fn BootedFunc) RunnerOption {
	return func(r *Runner) { r.booted = fn }
}

// NewRunner builds a Runner over the store.
func NewRunner(store *Store, opts ...RunnerOption) *Runner {
	r := &Runner{store: store}
	for _, o := range opts {
		o(r)
	}
	return r
}

// NewBackend builds the full mock action backend: the real AXeBackend
// with this package's Runner substituted for the AXe/simctl processes.
func NewBackend(store *Store, opts ...RunnerOption) *actions.AXeBackend {
	return actions.NewAXe(actions.WithRunner(NewRunner(store, opts...)), actions.WithAXePath(AXeBin))
}

// notBooted mirrors simctl's shutdown-target stderr so the backend's
// error mapping surfaces the stable target_not_booted protocol error.
var notBooted = []byte("Unable to lookup in current state: Shutdown")

func (r *Runner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	return r.run(ctx, nil, name, args...)
}

// RunInput implements actions.InputRunner (simctl pbcopy).
func (r *Runner) RunInput(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	return r.run(ctx, stdin, name, args...)
}

func (r *Runner) run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "simctl" {
		return r.simctl(ctx, stdin, args[1:])
	}
	return r.axe(ctx, args)
}

// axe emulates the AXe CLI argv surface (`axe <sub> ... --udid <udid>`).
func (r *Runner) axe(ctx context.Context, args []string) ([]byte, []byte, error) {
	if len(args) == 0 {
		return nil, []byte("usage: axe <subcommand>"), fmt.Errorf("exit status 64")
	}
	sub := args[0]
	udid := flagVal(args, "--udid")
	if udid == "" {
		return nil, []byte("missing --udid"), fmt.Errorf("exit status 64")
	}
	if r.booted != nil && !r.booted(ctx, udid) {
		return nil, notBooted, fmt.Errorf("exit status 1")
	}
	app := r.store.App(udid)
	switch sub {
	case "describe-ui":
		out, err := app.DescribeUI()
		return out, nil, err
	case "tap":
		x, okX := numFlag(args, "-x")
		y, okY := numFlag(args, "-y")
		if !okX || !okY {
			return nil, []byte("missing -x/-y"), fmt.Errorf("exit status 64")
		}
		app.Tap(x, y)
		return nil, nil, nil
	case "swipe":
		x1, ok1 := numFlag(args, "--start-x")
		y1, ok2 := numFlag(args, "--start-y")
		x2, ok3 := numFlag(args, "--end-x")
		y2, ok4 := numFlag(args, "--end-y")
		if !ok1 || !ok2 || !ok3 || !ok4 {
			return nil, []byte("missing swipe coordinates"), fmt.Errorf("exit status 64")
		}
		app.Swipe(x1, y1, x2, y2)
		return nil, nil, nil
	case "type":
		if len(args) < 2 {
			return nil, []byte("missing text"), fmt.Errorf("exit status 64")
		}
		app.Type(args[1])
		return nil, nil, nil
	case "key", "key-sequence", "button":
		// Accepted with no synthetic effect: the mock screen has no
		// hardware-button or raw-keycode transitions.
		return nil, nil, nil
	case "key-combo":
		// The only chord the backend sends is Cmd-V (the paste typing
		// strategy): deliver the pasteboard to the focused field.
		app.Type(r.store.Pasteboard(udid))
		return nil, nil, nil
	case "screenshot":
		path := flagVal(args, "--output")
		if path == "" {
			return nil, []byte("missing --output"), fmt.Errorf("exit status 64")
		}
		png, err := app.RenderPNG()
		if err != nil {
			return nil, []byte(err.Error()), fmt.Errorf("exit status 1")
		}
		return nil, nil, os.WriteFile(path, png, 0o600)
	}
	return nil, []byte("unknown subcommand " + sub), fmt.Errorf("exit status 64")
}

// simctl emulates the `xcrun simctl <sub> ...` argv surface.
func (r *Runner) simctl(ctx context.Context, stdin []byte, args []string) ([]byte, []byte, error) {
	if len(args) == 0 {
		return nil, []byte("usage: simctl <subcommand>"), fmt.Errorf("exit status 64")
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "install":
		return nil, nil, nil
	case "launch":
		var udid, bundle string
		for _, a := range rest {
			if len(a) > 2 && a[:2] == "--" {
				continue
			}
			if udid == "" {
				udid = a
			} else if bundle == "" {
				bundle = a
			}
		}
		if udid == "" || bundle == "" {
			return nil, []byte("usage: simctl launch <udid> <bundle>"), fmt.Errorf("exit status 64")
		}
		if r.booted != nil && !r.booted(ctx, udid) {
			return nil, notBooted, fmt.Errorf("exit status 1")
		}
		pid := r.store.App(udid).Launch()
		return []byte(fmt.Sprintf("%s: %d\n", bundle, pid)), nil, nil
	case "terminate":
		if len(rest) < 2 {
			return nil, []byte("usage: simctl terminate <udid> <bundle>"), fmt.Errorf("exit status 64")
		}
		if r.booted != nil && !r.booted(ctx, rest[0]) {
			return nil, notBooted, fmt.Errorf("exit status 1")
		}
		return nil, nil, nil
	case "pbcopy":
		if len(rest) < 1 {
			return nil, []byte("usage: simctl pbcopy <udid>"), fmt.Errorf("exit status 64")
		}
		r.store.SetPasteboard(rest[0], string(stdin))
		return nil, nil, nil
	case "pbpaste":
		if len(rest) < 1 {
			return nil, []byte("usage: simctl pbpaste <udid>"), fmt.Errorf("exit status 64")
		}
		return []byte(r.store.Pasteboard(rest[0])), nil, nil
	case "io":
		// io <udid> screenshot <path>
		if len(rest) < 3 || rest[1] != "screenshot" {
			return nil, []byte("usage: simctl io <udid> screenshot <path>"), fmt.Errorf("exit status 64")
		}
		if r.booted != nil && !r.booted(ctx, rest[0]) {
			return nil, notBooted, fmt.Errorf("exit status 1")
		}
		png, err := r.store.App(rest[0]).RenderPNG()
		if err != nil {
			return nil, []byte(err.Error()), fmt.Errorf("exit status 1")
		}
		return nil, nil, os.WriteFile(rest[2], png, 0o600)
	}
	return nil, []byte("unknown simctl subcommand " + sub), fmt.Errorf("exit status 64")
}

// flagVal returns the value following a flag, or "".
func flagVal(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// numFlag returns the numeric value following a flag.
func numFlag(args []string, flag string) (float64, bool) {
	s := flagVal(args, flag)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
