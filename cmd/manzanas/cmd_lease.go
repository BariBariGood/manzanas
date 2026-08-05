package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// cmdLease implements `manzanas lease acquire|wait|renew|release|ls`.
func cmdLease(ctx context.Context, app *appEnv, args []string) error {
	sub, err := requireArg(args, 0, "lease subcommand (acquire|wait|renew|release|ls)")
	if err != nil {
		return err
	}
	switch sub {
	case "acquire":
		return leaseAcquire(ctx, app, args[1:])
	case "wait":
		return leaseWait(ctx, app, args[1:])
	case "renew":
		return leaseRenew(ctx, app, args[1:])
	case "release":
		return leaseRelease(ctx, app, args[1:])
	case "ls":
		return leaseLs(ctx, app, args[1:])
	default:
		return fmt.Errorf("unknown lease subcommand %q", sub)
	}
}

// leaseAcquire maps to POST /v0/leases; --wait polls GET /v0/leases/{id}
// surfacing queue position until the lease is active.
func leaseAcquire(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("lease acquire")
	labels := fs.String("labels", "", "comma-separated labels, e.g. ios26,iphone-17-pro")
	udid := fs.String("udid", "", "pin the lease to this target UDID")
	agentID := fs.String("agent", "", "agent ID (default $USER)")
	purpose := fs.String("purpose", "", "freeform purpose")
	ttl := fs.Int("ttl", 0, "TTL seconds (default 300, max 3600)")
	reset := fs.String("reset", "", "auto-reset on release/expiry: none, erase, or snapshot:<name>")
	recordOpt := fs.String("record", "", "auto-record the lease: video (mp4 lands in the run journal at lease end)")
	wait := fs.Bool("wait", false, "if queued, wait until the lease is granted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *agentID == "" {
		*agentID = os.Getenv("USER")
		if *agentID == "" {
			*agentID = "manzanas-cli"
		}
	}
	var labelList []string
	for _, l := range strings.Split(*labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			labelList = append(labelList, l)
		}
	}
	l, err := app.client.AcquireLease(ctx, proto.AcquireLeaseRequest{
		Labels: labelList, UDID: *udid, AgentID: *agentID, Purpose: *purpose,
		TTLSeconds: *ttl, Reset: *reset, Record: *recordOpt,
	})
	if err != nil {
		return err
	}
	if l.State == proto.LeaseQueued && *wait {
		queuedID := l.ID
		fmt.Fprintf(app.stderr, "lease %s queued at position %d, waiting...\n", queuedID, l.QueuePosition)
		l, err = app.client.WaitForLease(ctx, queuedID, 2*time.Second, func(q proto.Lease) {
			fmt.Fprintf(app.stderr, "still queued, position %d\n", q.QueuePosition)
		})
		if err != nil {
			// Interrupted or failed while queued: best-effort release so
			// the queue isn't blocked for other agents.
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, rerr := app.client.ReleaseLease(rctx, queuedID); rerr == nil {
				fmt.Fprintf(app.stderr, "released queued lease %s\n", queuedID)
			} else {
				fmt.Fprintf(app.stderr, "failed to release queued lease %s: %v\n", queuedID, rerr)
			}
			return err
		}
	}
	return app.emit(l, func(w io.Writer) { printLease(w, l) })
}

// leaseWait resumes waiting on an already-queued lease, polling
// GET /v0/leases/{id} (which also keeps its queue-wait deadline fresh)
// until it becomes active.
func leaseWait(ctx context.Context, app *appEnv, args []string) error {
	id, err := requireArg(args, 0, "lease ID")
	if err != nil {
		return err
	}
	fs := app.newFlagSet("lease wait")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	l, err := app.client.WaitForLease(ctx, id, 2*time.Second, func(q proto.Lease) {
		fmt.Fprintf(app.stderr, "still queued, position %d\n", q.QueuePosition)
	})
	if err != nil {
		return err
	}
	return app.emit(l, func(w io.Writer) { printLease(w, l) })
}

func leaseRenew(ctx context.Context, app *appEnv, args []string) error {
	id, err := requireArg(args, 0, "lease ID")
	if err != nil {
		return err
	}
	fs := app.newFlagSet("lease renew")
	ttl := fs.Int("ttl", 0, "new TTL seconds (default: original TTL)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	l, err := app.client.RenewLease(ctx, id, *ttl)
	if err != nil {
		return err
	}
	return app.emit(l, func(w io.Writer) { printLease(w, l) })
}

func leaseRelease(ctx context.Context, app *appEnv, args []string) error {
	id, err := requireArg(args, 0, "lease ID")
	if err != nil {
		return err
	}
	fs := app.newFlagSet("lease release")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	l, err := app.client.ReleaseLease(ctx, id)
	if err != nil {
		return err
	}
	return app.emit(l, func(w io.Writer) { printLease(w, l) })
}

func leaseLs(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("lease ls")
	if err := fs.Parse(args); err != nil {
		return err
	}
	leases, err := app.client.ListLeases(ctx)
	if err != nil {
		return err
	}
	return app.emit(map[string]any{"leases": leases}, func(w io.Writer) {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATE\tTARGET\tAGENT\tLABELS\tEXPIRES")
		for _, l := range leases {
			exp := ""
			if l.ExpiresAt != nil {
				exp = l.ExpiresAt.Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				l.ID, l.State, l.TargetUDID, l.AgentID, strings.Join(l.Labels, ","), exp)
		}
		tw.Flush()
	})
}

func printLease(w io.Writer, l proto.Lease) {
	switch l.State {
	case proto.LeaseActive:
		exp := "unknown"
		if l.ExpiresAt != nil {
			exp = l.ExpiresAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "lease %s active on %s (ttl %ds, expires %s)\n",
			l.ID, l.TargetUDID, l.TTLSeconds, exp)
	case proto.LeaseQueued:
		fmt.Fprintf(w, "lease %s queued at position %d (run `manzanas lease wait %s` to hold your place until it's granted)\n",
			l.ID, l.QueuePosition, l.ID)
	default:
		fmt.Fprintf(w, "lease %s %s\n", l.ID, l.State)
	}
}
