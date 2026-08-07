package mcp

import (
	"context"
	"fmt"

	"github.com/BariBariGood/manzanas/internal/journal"
)

func toolJournalExport() Tool {
	return Tool{
		Name: "journal_export",
		Description: "Export a run's journal as PR-ready evidence. The run_id is the lease_id " +
			"of the run (every operation under a lease is journaled automatically). " +
			"format \"md\" (default) returns a self-contained markdown summary — run metadata, " +
			"an ordered step table with failures highlighted, and artifact references — ready to " +
			"paste into a PR comment or description. format \"json\" returns the raw export " +
			"(meta + full entry list + failure count). Works after the lease has been released; " +
			"a 501 error means this daemon runs with the journal disabled, and a 404 means the " +
			"run is unknown (check the lease_id, or the run may have been garbage-collected).",
		InputSchema: schema(map[string]map[string]any{
			"run_id": {"type": "string", "description": "the run to export; equal to the lease_id of the run"},
			"format": {"type": "string", "enum": []string{"md", "json"}, "description": "default md"},
		}, "run_id"),
		Call: func(ctx context.Context, s *Server, args map[string]any) ([]map[string]any, error) {
			runID, err := reqStr(args, "run_id")
			if err != nil {
				return nil, err
			}
			format := str(args, "format")
			if format == "" {
				format = "md"
			}
			if format != "md" && format != "json" {
				return nil, fmt.Errorf("invalid format %q: use \"md\" or \"json\"", format)
			}
			meta, entries, err := s.client.JournalExport(ctx, runID)
			if err != nil {
				return nil, err
			}
			doc := journal.BuildExport(runID, meta, entries)
			if format == "json" {
				return jsonContent(doc)
			}
			return textContent(doc.Markdown()), nil
		},
	}
}
