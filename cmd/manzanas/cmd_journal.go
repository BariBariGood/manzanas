package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/BariBariGood/manzanas/internal/journal"
)

// cmdJournal implements `manzanas journal tail|upload`.
func cmdJournal(ctx context.Context, app *appEnv, args []string) error {
	sub, err := requireArg(args, 0, "journal subcommand (tail|upload)")
	if err != nil {
		return err
	}
	switch sub {
	case "tail":
		return journalTail(ctx, app, args[1:])
	case "upload":
		return journalUpload(ctx, app, args[1:])
	default:
		return fmt.Errorf("unknown journal subcommand %q", sub)
	}
}

// journalTail maps to GET /v0/journal/{run_id}: prints existing entries,
// then with --follow polls for new ones.
func journalTail(ctx context.Context, app *appEnv, args []string) error {
	runID, err := requireArg(args, 0, "RUN_ID")
	if err != nil {
		return err
	}
	fs := app.newFlagSet("journal tail")
	follow := fs.Bool("follow", false, "keep polling for new entries")
	limit := fs.Int("limit", 100, "entries per page")
	if err := fs.Parse(args[1:]); err != nil {
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

// journalUpload maps to POST /v0/journal/{run_id}/artifacts?name=&kind=,
// once per file: `manzanas journal upload RUN_ID FILE... [--kind screenshot]`.
// The run's lease must still be active (finished runs are immutable).
func journalUpload(ctx context.Context, app *appEnv, args []string) error {
	runID, err := requireArg(args, 0, "RUN_ID")
	if err != nil {
		return err
	}
	var files []string
	rest := args[1:]
	for len(rest) > 0 && rest[0] != "" && rest[0][0] != '-' {
		files = append(files, rest[0])
		rest = rest[1:]
	}
	fs := app.newFlagSet("journal upload")
	kind := fs.String("kind", "screenshot", "artifact kind: observation, screenshot, or video")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	files = append(files, fs.Args()...)
	if len(files) == 0 {
		return fmt.Errorf("missing FILE argument(s)")
	}
	type uploaded struct {
		File     string              `json:"file"`
		Artifact journal.ArtifactRef `json:"artifact"`
	}
	var results []uploaded
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		ref, err := app.client.JournalArtifactUpload(ctx, runID, filepath.Base(file), *kind, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("upload %s: %w", file, err)
		}
		results = append(results, uploaded{File: file, Artifact: ref})
	}
	return app.emit(map[string]any{"run_id": runID, "uploaded": results}, func(w io.Writer) {
		for _, u := range results {
			fmt.Fprintf(w, "%s -> %s (sha256 %s, %d bytes)\n",
				u.File, u.Artifact.Path, u.Artifact.SHA256, u.Artifact.Bytes)
			fmt.Fprintf(w, "  cite as: run %s, artifact %s\n", runID, u.Artifact.Path)
		}
	})
}
