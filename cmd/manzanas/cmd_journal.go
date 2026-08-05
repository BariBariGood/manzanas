package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/BariBariGood/manzanas/internal/journal"
)

// cmdJournal implements `manzanas journal tail RUN_ID`
// (GET /v0/journal/{run_id}): prints existing entries, then with --follow
// polls for new ones.
func cmdJournal(ctx context.Context, app *appEnv, args []string) error {
	sub, err := requireArg(args, 0, "journal subcommand (tail)")
	if err != nil {
		return err
	}
	if sub != "tail" {
		return fmt.Errorf("unknown journal subcommand %q", sub)
	}
	runID, err := requireArg(args[1:], 0, "RUN_ID")
	if err != nil {
		return err
	}
	fs := app.newFlagSet("journal tail")
	follow := fs.Bool("follow", false, "keep polling for new entries")
	limit := fs.Int("limit", 100, "entries per page")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("journal tail: unexpected argument %q", fs.Arg(0))
	}

	var fromSeq int64
	for {
		entries, nextSeq, err := app.client.JournalRead(ctx, runID, fromSeq, *limit)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if app.json {
				if err := json.NewEncoder(app.stdout).Encode(e); err != nil {
					return err
				}
			} else {
				printEntry(app.stdout, e)
			}
			if e.Ref.Seq >= fromSeq {
				fromSeq = e.Ref.Seq + 1
			}
		}
		// next_seq is the daemon's authoritative cursor (0 = end of run);
		// the daemon clamps limit server-side so page size is no signal.
		if nextSeq > 0 {
			fromSeq = nextSeq
			continue
		}
		if !*follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

func printEntry(w io.Writer, e journal.Entry) {
	b, _ := json.Marshal(e.Payload)
	fmt.Fprintf(w, "%d %s %s\n", e.Ref.Seq, e.Kind, string(b))
}
