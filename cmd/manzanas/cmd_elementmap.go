package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/BariBariGood/manzanas/internal/elementmap"
)

// cmdElementMap implements `manzanas elementmap ls|rm` over the daemon's
// element-map endpoints (PROTOCOL.md §5.4).
func cmdElementMap(ctx context.Context, app *appEnv, args []string) error {
	sub, err := requireArg(args, 0, "elementmap subcommand (ls|rm)")
	if err != nil {
		return err
	}
	switch sub {
	case "ls":
		return elementMapLs(ctx, app, args[1:])
	case "rm":
		return elementMapRm(ctx, app, args[1:])
	default:
		return fmt.Errorf("unknown elementmap subcommand %q", sub)
	}
}

// elementMapLs maps to GET /v0/elementmap.
func elementMapLs(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("elementmap ls")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("elementmap ls: unexpected argument %q", fs.Arg(0))
	}
	list, err := app.client.ElementMapList(ctx)
	if err != nil {
		return err
	}
	if list == nil {
		list = []elementmap.Entry{}
	}
	return app.emit(map[string]any{"elements": list}, func(w io.Writer) {
		if len(list) == 0 {
			fmt.Fprintln(w, "no elements")
			return
		}
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "BUNDLE\tNAME\tCLASS\tROLE\tFRAME\tLEARNED")
		for _, e := range list {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.0f,%.0f %.0fx%.0f\t%s\n",
				e.BundleID, e.Name, e.DeviceClass, e.Role,
				e.Frame.X, e.Frame.Y, e.Frame.W, e.Frame.H,
				e.LearnedAt.Format("2006-01-02 15:04:05"))
		}
		tw.Flush()
	})
}

// elementMapRm maps to DELETE /v0/elementmap/{bundle_id}?name=...
func elementMapRm(ctx context.Context, app *appEnv, args []string) error {
	bundleID, err := requireArg(args, 0, "BUNDLE_ID")
	if err != nil {
		return err
	}
	name, err := requireArg(args, 1, "NAME")
	if err != nil {
		return err
	}
	fs := app.newFlagSet("elementmap rm")
	class := fs.String("class", "", "remove only this device class (e.g. axe/390); default removes all classes")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("elementmap rm: unexpected argument %q", fs.Arg(0))
	}
	removed, err := app.client.ElementMapRemove(ctx, bundleID, name, *class)
	if err != nil {
		return err
	}
	return app.emit(map[string]any{"bundle_id": bundleID, "name": name, "removed": removed}, func(w io.Writer) {
		fmt.Fprintf(w, "removed %d element(s) for %s %q\n", removed, bundleID, name)
	})
}
