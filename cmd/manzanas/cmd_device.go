package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/BariBariGood/manzanas/internal/devices"
	"github.com/BariBariGood/manzanas/proto"
)

// cmdDevice implements `manzanas device onboard|config`.
func cmdDevice(ctx context.Context, app *appEnv, args []string) error {
	sub, err := requireArg(args, 0, "onboard|config")
	if err != nil {
		return err
	}
	switch sub {
	case "onboard":
		return cmdDeviceOnboard(ctx, app, args[1:])
	case "config":
		return cmdDeviceConfig(ctx, app, args[1:])
	default:
		return fmt.Errorf("unknown device subcommand %q (want onboard or config)", sub)
	}
}

// cmdDeviceConfig implements `manzanas device config` (GET /v0/devices).
func cmdDeviceConfig(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("device config")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := app.client.DevicesGet(ctx)
	if err != nil {
		return err
	}
	return app.emit(cfg, func(w io.Writer) {
		fmt.Fprintf(w, "devices enabled: %t\n", cfg.Enabled)
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "UDID\tBACKEND\tWDA URL / MIRROR SOCKET\tLAUNCH\tFORWARD")
		for _, udid := range devices.UDIDs(cfg) {
			if m, ok := cfg.Mirror[udid]; ok {
				socket := m.Socket
				if socket == "" {
					socket = devices.DefaultMirrorSocket()
				}
				fmt.Fprintf(tw, "%s\tmirror\t%s\t-\t-\n", udid, socket)
				continue
			}
			d := cfg.WDA[udid]
			fmt.Fprintf(tw, "%s\twda\t%s\t%s\t%s\n", udid, orDash(d.URL), orDash(d.Launch), orDash(d.Forward))
		}
		tw.Flush()
	})
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// cmdDeviceOnboard implements `manzanas device onboard <udid>`: one
// command from a paired phone to a leasable /v0/targets entry with
// working HID — build+sign WDA headlessly, produce the daemon config
// (WDA URL + supervised xctestrun launch + supervised usbmux forward),
// then apply it to the daemon and/or write it to a config file.
func cmdDeviceOnboard(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("device onboard")
	var opts devices.OnboardOptions
	fs.StringVar(&opts.WDASource, "wda-src", "", "WebDriverAgent checkout (default: cached clone under ~/.manzanas/wda, fetched if missing)")
	fs.StringVar(&opts.BundleID, "wda-bundle-id", devices.DefaultWDABundleID, "manzanas-owned bundle id to build WDA under (avoids the upstream project's hardcoded team)")
	fs.StringVar(&opts.Team, "team", os.Getenv("APPLE_TEAM_ID"), "Apple Development team id (DEVELOPMENT_TEAM; default $APPLE_TEAM_ID)")
	fs.StringVar(&opts.Keychain, "keychain", "", "dedicated signing keychain for headless codesign over SSH (must be unlocked with codesign in its key partition list)")
	fs.StringVar(&opts.ASCKeyPath, "asc-key-path", os.Getenv("ASC_API_KEY_PATH"), "App Store Connect API key .p8 for headless provisioning (default $ASC_API_KEY_PATH)")
	fs.StringVar(&opts.ASCKeyID, "asc-key-id", os.Getenv("ASC_API_KEY_ID"), "ASC API key id (default $ASC_API_KEY_ID)")
	fs.StringVar(&opts.ASCIssuerID, "asc-issuer-id", os.Getenv("ASC_API_ISSUER_ID"), "ASC API issuer id (default $ASC_API_ISSUER_ID)")
	fs.StringVar(&opts.Forward, "forward", "8100:8100", "usbmux port forward <local>:<remote> the daemon supervises")
	fs.StringVar(&opts.DerivedData, "derived-data", "", "xcodebuild derived-data dir (default ~/.manzanas/wda-derived-<udid>)")
	fs.BoolVar(&opts.SkipBuild, "skip-build", false, "reuse an existing device .xctestrun from derived data instead of rebuilding")
	apply := fs.Bool("apply", false, "apply the resulting config to the daemon (POST /v0/devices, merged over its current config) and wait for the device in /v0/targets")
	configOut := fs.String("config-out", "", "write the resulting config to FILE (merged over the file's current contents)")
	wait := fs.Duration("wait", 3*time.Minute, "how long --apply waits for the device to appear in /v0/targets")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	udid, err := requireArg(rest, 0, "device UDID")
	if err != nil {
		return err
	}
	// Re-parse so flags after the UDID work too (`device onboard UDID
	// --apply`); flag.Parse stops at the first positional argument.
	if err := fs.Parse(rest[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("device onboard: unexpected argument %q", fs.Arg(0))
	}
	opts.UDID = udid
	if opts.ASCKeyPath != "" || opts.ASCKeyID != "" || opts.ASCIssuerID != "" {
		if opts.ASCKeyPath == "" || opts.ASCKeyID == "" || opts.ASCIssuerID == "" {
			return errors.New("device onboard: --asc-key-path, --asc-key-id, and --asc-issuer-id must be set together")
		}
	}

	// Pre-flight --apply before the (slow) WDA build: a daemon that is
	// down, or one without runtime device config, fails here instead of
	// after minutes of xcodebuild. POST /v0/devices additionally requires
	// the daemon to run with --auth-token (403 otherwise).
	if *apply {
		if app.token == "" {
			return errors.New("device onboard --apply: POST /v0/devices requires a bearer token; pass --token (or set $MANZANAS_TOKEN) matching the daemon's --auth-token")
		}
		if _, err := app.client.DevicesGet(ctx); err != nil {
			return fmt.Errorf("device onboard --apply: %w", err)
		}
	}

	ob := devices.NewOnboarder(app.stderr)
	res, err := ob.Onboard(ctx, opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(app.stderr, "built %s\n", res.XCTestRun)

	if *configOut != "" {
		merged := res.Config
		existing, lerr := devices.Load(*configOut)
		switch {
		case lerr == nil:
			merged = mergeDevices(existing, res.Config)
		case !os.IsNotExist(lerr):
			// Only a missing file falls through to a fresh write; an
			// unreadable/invalid one would silently lose its other devices.
			return fmt.Errorf("device onboard --config-out: refusing to overwrite %s: %w", *configOut, lerr)
		}
		if err := devices.Validate(merged); err != nil {
			return fmt.Errorf("device onboard --config-out: refusing to write an invalid config to %s (pass a distinct --forward for each device): %w", *configOut, err)
		}
		if err := devices.WriteFile(*configOut, merged); err != nil {
			return err
		}
		fmt.Fprintf(app.stderr, "wrote %s (SIGHUP the daemon to pick it up)\n", *configOut)
	}

	if *apply {
		cur, err := app.client.DevicesGet(ctx)
		if err != nil {
			return fmt.Errorf("device onboard --apply: %w", err)
		}
		merged := mergeDevices(cur, res.Config)
		if _, err := app.client.DevicesApply(ctx, merged); err != nil {
			return fmt.Errorf("device onboard --apply: %w", err)
		}
		fmt.Fprintf(app.stderr, "applied to daemon; waiting for %s in /v0/targets…\n", udid)
		if err := waitForTarget(ctx, app, udid, *wait); err != nil {
			return err
		}
		fmt.Fprintf(app.stderr, "device %s is a leasable target\n", udid)
	}

	return app.emit(res.Config, func(w io.Writer) {
		d := res.Config.WDA[udid]
		fmt.Fprintln(w, "daemon config for this device:")
		fmt.Fprintf(w, "  flags:  --devices --device-wda %s=%s --device-wda-launch %s=%s --device-wda-forward %s=%s\n",
			udid, d.URL, udid, d.Launch, udid, d.Forward)
		fmt.Fprintf(w, "  config: {\"enabled\":true,\"wda\":{%q:{\"url\":%q,\"launch\":%q,\"forward\":%q}}}\n",
			udid, d.URL, d.Launch, d.Forward)
	})
}

// mergeDevices overlays add onto base (per-device replace; enabled wins
// from add).
func mergeDevices(base, add proto.DevicesConfig) proto.DevicesConfig {
	out := proto.DevicesConfig{Enabled: base.Enabled || add.Enabled,
		WDA: map[string]proto.DeviceWDAConfig{}}
	for u, d := range base.WDA {
		out.WDA[u] = d
	}
	for u, d := range add.WDA {
		out.WDA[u] = d
	}
	if len(base.Mirror)+len(add.Mirror) > 0 {
		out.Mirror = map[string]proto.DeviceMirrorConfig{}
		for u, d := range base.Mirror {
			out.Mirror[u] = d
		}
		for u, d := range add.Mirror {
			out.Mirror[u] = d
		}
		// Per-device replace across backends: a device added on one
		// side must not keep its entry on the other (Validate rejects a
		// UDID present in both wda and mirror).
		for u := range add.WDA {
			delete(out.Mirror, u)
		}
		for u := range add.Mirror {
			delete(out.WDA, u)
		}
		if len(out.Mirror) == 0 {
			out.Mirror = nil
		}
	}
	return out
}

// waitForTarget polls /v0/targets until udid shows up (any state: a
// paired-but-disconnected device is still a valid, leasable target).
func waitForTarget(ctx context.Context, app *appEnv, udid string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var lastErr error
	for {
		targets, err := app.client.ListTargets(ctx)
		lastErr = err
		if err == nil {
			for _, t := range targets {
				if t.UDID == udid {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("device %s did not appear in /v0/targets within %s; last /v0/targets error: %w", udid, budget, lastErr)
			}
			return fmt.Errorf("device %s did not appear in /v0/targets within %s (is it paired? check `xcrun devicectl list devices`)", udid, budget)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
