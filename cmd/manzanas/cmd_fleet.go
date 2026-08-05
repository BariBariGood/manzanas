package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// cmdFleet implements `manzanas fleet hosts|placements|hints` against a
// broker (point --daemon at the broker address).
func cmdFleet(ctx context.Context, app *appEnv, args []string) error {
	sub, err := requireArg(args, 0, "hosts|placements|hints")
	if err != nil {
		return err
	}
	switch sub {
	case "hosts":
		return cmdFleetHosts(ctx, app, args[1:])
	case "placements":
		return cmdFleetPlacements(ctx, app, args[1:])
	case "hints":
		return cmdFleetHints(ctx, app, args[1:])
	default:
		return fmt.Errorf("unknown fleet subcommand %q (want hosts, placements, or hints)", sub)
	}
}

// cmdFleetHosts implements `manzanas fleet hosts` (GET /v0/fleet/hosts).
func cmdFleetHosts(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("fleet hosts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("fleet hosts: unexpected argument %q", fs.Arg(0))
	}
	hosts, err := app.client.FleetHosts(ctx)
	if err != nil {
		return err
	}
	return app.emit(map[string]any{"hosts": hosts}, func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "HOST\tUP\tTARGETS\tLEASES\tPARKED\tRUNNING\tGATES")
		for _, h := range hosts {
			parked, running, gates := "-", "-", "-"
			if h.Stats != nil {
				parked = fmt.Sprint(h.Stats.Parked)
				running = fmt.Sprint(h.Stats.Running)
				gates = gateNames(h.Stats.Gates.LoadOK, h.Stats.Gates.DiskOK)
			}
			fmt.Fprintf(tw, "%s\t%t\t%d\t%d\t%s\t%s\t%s\n",
				h.Name, h.Up, h.Targets, h.ActiveLeases, parked, running, gates)
		}
		tw.Flush()
	})
}

func gateNames(loadOK, diskOK bool) string {
	if loadOK && diskOK {
		return "ok"
	}
	var bad []string
	if !loadOK {
		bad = append(bad, "load")
	}
	if !diskOK {
		bad = append(bad, "disk")
	}
	return "FAIL:" + strings.Join(bad, ",")
}

// cmdFleetPlacements implements `manzanas fleet placements [-n N]`
// (GET /v0/fleet/placements): why each recent lease went where it did.
func cmdFleetPlacements(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("fleet placements")
	n := fs.Int("n", 20, "max decisions to show (0 = all retained)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("fleet placements: unexpected argument %q", fs.Arg(0))
	}
	placements, err := app.client.FleetPlacements(ctx, *n)
	if err != nil {
		return err
	}
	return app.emit(map[string]any{"placements": placements}, func(w io.Writer) {
		for _, p := range placements {
			target := p.Host
			if target == "" {
				target = "-"
			}
			fmt.Fprintf(w, "%s  %s → %s (%s", p.At.Format("15:04:05"),
				strings.Join(p.Labels, ","), target, p.Outcome)
			if p.Tier != "" {
				fmt.Fprintf(w, ", tier=%s", p.Tier)
			}
			if p.LeaseID != "" {
				fmt.Fprintf(w, ", lease=%s", p.LeaseID)
			}
			fmt.Fprintln(w, ")")
			for _, c := range p.Candidates {
				extra := ""
				if c.HasStats {
					extra = fmt.Sprintf(" parked=%d headroom=%s", c.Parked, headroomStr(c.Headroom))
				}
				fmt.Fprintf(w, "    %-12s tier=%-9s load=%d warm_match=%t warm_idle=%d%s\n",
					c.Host, c.Tier, c.EffectiveLoad, c.WarmMatch, c.WarmIdle, extra)
			}
		}
	})
}

func headroomStr(h int) string {
	if h < 0 {
		return "uncapped"
	}
	return fmt.Sprint(h)
}

// cmdFleetHints implements `manzanas fleet hints` (GET /v0/fleet/hints):
// the broker's current warm-pool rebalancing advice per host.
func cmdFleetHints(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("fleet hints")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("fleet hints: unexpected argument %q", fs.Arg(0))
	}
	window, hosts, err := app.client.FleetHints(ctx)
	if err != nil {
		return err
	}
	return app.emit(map[string]any{"window_seconds": window, "hosts": hosts}, func(w io.Writer) {
		if len(hosts) == 0 {
			fmt.Fprintf(w, "no rebalancing hints (window %ds)\n", window)
			return
		}
		fmt.Fprintf(w, "window %ds\n", window)
		for _, hh := range hosts {
			for _, c := range hh.Classes {
				fmt.Fprintf(w, "%s: %s %s — %s\n",
					hh.Host, c.Action, strings.Join(c.Labels, ","), c.Reason)
			}
		}
	})
}
