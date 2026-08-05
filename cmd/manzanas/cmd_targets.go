package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

// cmdTargets implements `manzanas targets` (GET /v0/targets), with
// client-side --kind and --labels filters.
func cmdTargets(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("targets")
	kind := fs.String("kind", "", "only targets of this kind: simulator or device")
	labels := fs.String("labels", "", "only targets carrying all of these comma-separated labels")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var want []string
	for _, l := range strings.Split(*labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			want = append(want, l)
		}
	}
	targets, err := app.client.ListTargets(ctx)
	if err != nil {
		return err
	}
	filtered := targets[:0]
	for _, t := range targets {
		if *kind != "" && string(t.Kind) != *kind {
			continue
		}
		if !hasAllLabels(t.Labels, want) {
			continue
		}
		filtered = append(filtered, t)
	}
	targets = filtered
	return app.emit(map[string]any{"targets": targets}, func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "UDID\tKIND\tNAME\tRUNTIME\tSTATE\tLABELS")
		for _, t := range targets {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				t.UDID, t.Kind, t.Name, t.Runtime, t.State, strings.Join(t.Labels, ","))
		}
		tw.Flush()
	})
}

// hasAllLabels reports whether have contains every label in want.
func hasAllLabels(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// cmdBoot implements `manzanas boot UDID --lease ID`
// (POST /v0/targets/{udid}/boot).
func cmdBoot(ctx context.Context, app *appEnv, args []string) error {
	return bootOrShutdown(ctx, app, args, "boot")
}

// cmdShutdownTarget implements `manzanas shutdown UDID --lease ID`
// (POST /v0/targets/{udid}/shutdown).
func cmdShutdownTarget(ctx context.Context, app *appEnv, args []string) error {
	return bootOrShutdown(ctx, app, args, "shutdown")
}

func bootOrShutdown(ctx context.Context, app *appEnv, args []string, op string) error {
	fs := app.newFlagSet(op)
	leaseID := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID (or $MANZANAS_LEASE)")
	udid, err := requireArg(args, 0, "UDID")
	if err != nil {
		return err
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *leaseID == "" {
		return fmt.Errorf("--lease (or $MANZANAS_LEASE) is required for %s", op)
	}
	call := app.client.BootTarget
	if op == "shutdown" {
		call = app.client.ShutdownTarget
	}
	t, err := call(ctx, udid, *leaseID)
	if err != nil {
		return err
	}
	return app.emit(t, func(w io.Writer) {
		fmt.Fprintf(w, "%s %s: state=%s (poll `manzanas targets` until Booted/Shutdown)\n",
			op, t.UDID, t.State)
	})
}
