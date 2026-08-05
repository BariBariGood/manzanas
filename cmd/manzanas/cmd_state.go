package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// cmdState implements `manzanas state snapshot|restore|fixture`
// (POST /v0/state/snapshots|restore|fixtures; the target UDID is derived
// server-side from the lease).
func cmdState(ctx context.Context, app *appEnv, args []string) error {
	sub, err := requireArg(args, 0, "state subcommand (snapshot|restore|fixture)")
	if err != nil {
		return err
	}
	switch sub {
	case "snapshot":
		return stateSnapshot(ctx, app, args[1:])
	case "restore":
		return stateRestore(ctx, app, args[1:])
	case "fixture":
		return stateFixture(ctx, app, args[1:])
	default:
		return fmt.Errorf("unknown state subcommand %q", sub)
	}
}

// stateSnapshot: `manzanas state snapshot [--label L] --lease ID`.
func stateSnapshot(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("state snapshot")
	lease := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID")
	label := fs.String("label", "", "optional snapshot label")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("state snapshot: unexpected argument %q", fs.Arg(0))
	}
	if *lease == "" {
		return fmt.Errorf("state snapshot: --lease (or $MANZANAS_LEASE) is required")
	}
	info, err := app.client.StateSnapshot(ctx, *lease, *label)
	if err != nil {
		return err
	}
	return app.emit(info, func(w io.Writer) {
		fmt.Fprintln(w, info.ID)
	})
}

// stateRestore: `manzanas state restore SNAPSHOT [--reboot] --lease ID`
// (SNAPSHOT is a snapshot ID or label).
func stateRestore(ctx context.Context, app *appEnv, args []string) error {
	snapshot, err := requireArg(args, 0, "snapshot ID or label")
	if err != nil {
		return err
	}
	fs := app.newFlagSet("state restore")
	lease := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID")
	reboot := fs.Bool("reboot", false, "shutdown+restore+boot if the target is booted")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("state restore: unexpected argument %q", fs.Arg(0))
	}
	if *lease == "" {
		return fmt.Errorf("state restore: --lease (or $MANZANAS_LEASE) is required")
	}
	res, err := app.client.StateRestore(ctx, *lease, snapshot, *reboot)
	if err != nil {
		return err
	}
	return app.emit(res, func(w io.Writer) {
		fmt.Fprintln(w, "ok")
	})
}

// stateFixture applies a named fixture: `manzanas state fixture NAME
// [--payload JSON | --payload-file f.json] --lease ID`.
func stateFixture(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("state fixture")
	lease := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID")
	payloadJSON := fs.String("payload", "", "fixture payload as inline JSON")
	payloadFile := fs.String("payload-file", "", "fixture payload from a JSON file")
	if len(args) < 1 {
		return fmt.Errorf("state fixture: expected NAME")
	}
	if strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("state fixture: positional arguments must come before flags (got %q)", args[0])
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("state fixture: unexpected argument %q (expected NAME before flags)", fs.Arg(0))
	}
	if *lease == "" {
		return fmt.Errorf("state fixture: --lease (or $MANZANAS_LEASE) is required")
	}
	raw := []byte(*payloadJSON)
	if *payloadFile != "" {
		var err error
		raw, err = os.ReadFile(*payloadFile)
		if err != nil {
			return err
		}
	}
	var payload map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("state fixture: invalid payload JSON: %w", err)
		}
	}
	if err := app.client.StateFixture(ctx, *lease, args[0], payload); err != nil {
		return err
	}
	return app.emit(map[string]bool{"ok": true}, func(w io.Writer) {
		fmt.Fprintln(w, "ok")
	})
}
