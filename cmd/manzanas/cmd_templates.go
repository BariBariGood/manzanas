package main

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/BariBariGood/manzanas/internal/templates"
)

// cmdTemplates implements `manzanas templates ls|rm` over the daemon's
// template-library endpoints (PROTOCOL.md §5.4).
func cmdTemplates(ctx context.Context, app *appEnv, args []string) error {
	sub, err := requireArg(args, 0, "templates subcommand (ls|rm)")
	if err != nil {
		return err
	}
	switch sub {
	case "ls":
		return templatesLs(ctx, app, args[1:])
	case "rm":
		return templatesRm(ctx, app, args[1:])
	default:
		return fmt.Errorf("unknown templates subcommand %q", sub)
	}
}

// templatesLs maps to GET /v0/templates.
func templatesLs(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("templates ls")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("templates ls: unexpected argument %q", fs.Arg(0))
	}
	list, err := app.client.TemplatesList(ctx)
	if err != nil {
		return err
	}
	if list == nil {
		list = []templates.Entry{}
	}
	return app.emit(map[string]any{"templates": list}, func(w io.Writer) {
		if len(list) == 0 {
			fmt.Fprintln(w, "no templates")
			return
		}
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "BUNDLE\tNAME\tCLASS\tRECT\tWINDOW\tBYTES\tCREATED")
		for _, e := range list {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d,%d %dx%d\t%dx%d\t%d\t%s\n",
				e.BundleID, e.Name, e.DeviceClass,
				e.CropRect.X, e.CropRect.Y, e.CropRect.W, e.CropRect.H,
				e.WindowPx[0], e.WindowPx[1], e.Bytes,
				e.CreatedAt.Format("2006-01-02 15:04:05"))
		}
		tw.Flush()
	})
}

// templatesRm maps to DELETE /v0/templates/{bundle_id}/{name}.
func templatesRm(ctx context.Context, app *appEnv, args []string) error {
	bundleID, err := requireArg(args, 0, "BUNDLE_ID")
	if err != nil {
		return err
	}
	name, err := requireArg(args, 1, "NAME")
	if err != nil {
		return err
	}
	fs := app.newFlagSet("templates rm")
	class := fs.String("class", "", "remove only this device class (e.g. mirror/318); default removes all classes")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("templates rm: unexpected argument %q", fs.Arg(0))
	}
	removed, err := app.client.TemplatesRemove(ctx, bundleID, name, *class)
	if err != nil {
		return err
	}
	return app.emit(map[string]any{"bundle_id": bundleID, "name": name, "removed": removed}, func(w io.Writer) {
		fmt.Fprintf(w, "removed %d template(s) for %s/%s\n", removed, bundleID, name)
	})
}
