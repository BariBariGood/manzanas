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

// cmdJournal implements `manzanas journal tail|upload|export|to-spec`.
func cmdJournal(ctx context.Context, app *appEnv, args []string) error {
	sub, err := requireArg(args, 0, "journal subcommand (tail|upload|export|to-spec)")
	if err != nil {
		return err
	}
	switch sub {
	case "tail":
		return journalTail(ctx, app, args[1:])
	case "upload":
		return journalUpload(ctx, app, args[1:])
	case "export":
		return journalExport(ctx, app, args[1:])
	case "to-spec":
		return journalToSpec(ctx, app, args[1:])
	default:
		return fmt.Errorf("unknown journal subcommand %q", sub)
	}
}

// journalExport renders a run as PR-ready evidence: a self-contained
// markdown summary (or the raw JSON export) built from the run's metadata
// and full entry list. It reads from the daemon by default, or offline
// straight from a journal directory with --journal-dir.
func journalExport(ctx context.Context, app *appEnv, args []string) error {
	runID, err := requireArg(args, 0, "RUN_ID")
	if err != nil {
		return err
	}
	fs := app.newFlagSet("journal export")
	format := fs.String("format", "md", "output format: md or json")
	journalDir := fs.String("journal-dir", "", "read offline from this journal directory (e.g. ~/.manzanasd/journal) instead of the daemon")
	outPath := fs.String("o", "", "write to FILE instead of stdout")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("journal export: unexpected argument %q", fs.Arg(0))
	}
	if *format != "md" && *format != "json" {
		return fmt.Errorf("journal export: --format must be md or json, got %q", *format)
	}

	var meta journal.RunMeta
	var entries []journal.Entry
	if *journalDir != "" {
		// NewFileStore creates the directory (it serves the daemon's write
		// path); a read-only export must fail loudly on a mistyped one.
		if _, statErr := os.Stat(*journalDir); statErr != nil {
			return fmt.Errorf("journal export: open %s: %w", *journalDir, statErr)
		}
		store, err := journal.NewFileStore(*journalDir)
		if err != nil {
			return fmt.Errorf("journal export: open %s: %w", *journalDir, err)
		}
		if entries, err = store.Read(ctx, runID, 0, 0); err != nil {
			return fmt.Errorf("journal export: %w", err)
		}
		if meta, err = store.ReadMeta(runID); err != nil {
			return fmt.Errorf("journal export: %w", err)
		}
	} else if meta, entries, err = app.client.JournalExport(ctx, runID); err != nil {
		return err
	}

	doc := journal.BuildExport(runID, meta, entries)
	var rendered []byte
	if *format == "json" {
		if rendered, err = json.MarshalIndent(doc, "", "  "); err != nil {
			return err
		}
		rendered = append(rendered, '\n')
	} else {
		rendered = []byte(doc.Markdown())
	}
	if *outPath != "" {
		return os.WriteFile(*outPath, rendered, 0o644)
	}
	_, err = app.stdout.Write(rendered)
	return err
}

// journalToSpec exports a journaled run as a declarative YAML run-spec
// (record -> replay): each recorded action becomes a run step, ready for
// `manzanas run`. It reads from the daemon by default, or offline straight
// from a journal directory with --journal-dir, like journal export.
func journalToSpec(ctx context.Context, app *appEnv, args []string) error {
	runID, err := requireArg(args, 0, "RUN_ID")
	if err != nil {
		return err
	}
	fs := app.newFlagSet("journal to-spec")
	journalDir := fs.String("journal-dir", "", "read offline from this journal directory (e.g. ~/.manzanasd/journal) instead of the daemon")
	outPath := fs.String("o", "", "write to FILE instead of stdout")
	keepObs := fs.Bool("keep-observations", false, "keep pure-read steps (observe, screenshot, audit, wait_*) instead of dropping them")
	includeFailed := fs.Bool("include-failed", false, "keep the first failed step and everything after it")
	withExpectations := fs.Bool("with-expectations", false, "attach expect blocks (text set + DHash) from the run's recorded step expectations (record_expectations: true)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("journal to-spec: unexpected argument %q", fs.Arg(0))
	}

	meta, entries, err := journalLoad(ctx, app, runID, *journalDir, "journal to-spec")
	if err != nil {
		return err
	}
	spec, err := journal.ExportSpecYAML(meta, entries, journal.SpecOptions{
		KeepObservations: *keepObs,
		IncludeFailed:    *includeFailed,
		WithExpectations: *withExpectations,
	})
	if err != nil {
		return fmt.Errorf("journal to-spec: %w", err)
	}
	if *outPath != "" {
		return os.WriteFile(*outPath, spec, 0o644)
	}
	_, err = app.stdout.Write(spec)
	return err
}

// journalLoad fetches a run's metadata and full entry list, either from
// the daemon or offline from an on-disk journal directory.
func journalLoad(ctx context.Context, app *appEnv, runID, journalDir, op string) (journal.RunMeta, []journal.Entry, error) {
	if journalDir == "" {
		return app.client.JournalExport(ctx, runID)
	}
	// NewFileStore creates the directory (it serves the daemon's write
	// path); a read-only export must fail loudly on a mistyped one.
	if _, statErr := os.Stat(journalDir); statErr != nil {
		return journal.RunMeta{}, nil, fmt.Errorf("%s: open %s: %w", op, journalDir, statErr)
	}
	store, err := journal.NewFileStore(journalDir)
	if err != nil {
		return journal.RunMeta{}, nil, fmt.Errorf("%s: open %s: %w", op, journalDir, err)
	}
	entries, err := store.Read(ctx, runID, 0, 0)
	if err != nil {
		return journal.RunMeta{}, nil, fmt.Errorf("%s: %w", op, err)
	}
	meta, err := store.ReadMeta(runID)
	if err != nil {
		return journal.RunMeta{}, nil, fmt.Errorf("%s: %w", op, err)
	}
	return meta, entries, nil
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
