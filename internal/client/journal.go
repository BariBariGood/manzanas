package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/BariBariGood/manzanas/internal/journal"
)

// JournalRead calls GET /v0/journal/{run_id}?from_seq=&limit= and returns
// entries starting at fromSeq (inclusive) plus the daemon's next_seq
// cursor (0 when this page reached the end of the run).
func (c *Client) JournalRead(ctx context.Context, runID string, fromSeq int64, limit int) ([]journal.Entry, int64, error) {
	var out struct {
		Entries []journal.Entry `json:"entries"`
		NextSeq int64           `json:"next_seq"`
	}
	path := fmt.Sprintf("/v0/journal/%s?from_seq=%d&limit=%d",
		url.PathEscape(runID), fromSeq, limit)
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Entries, out.NextSeq, err
}

// JournalExport pages GET /v0/journal/{run_id} to the end of the run and
// returns the run's metadata plus its full seq-ordered entry list. Used by
// `manzanas journal export` and the MCP journal_export tool.
func (c *Client) JournalExport(ctx context.Context, runID string) (journal.RunMeta, []journal.Entry, error) {
	var meta journal.RunMeta
	var entries []journal.Entry
	var fromSeq int64
	for {
		var out struct {
			Meta    journal.RunMeta `json:"meta"`
			Entries []journal.Entry `json:"entries"`
			NextSeq int64           `json:"next_seq"`
		}
		path := fmt.Sprintf("/v0/journal/%s?from_seq=%d&limit=1000",
			url.PathEscape(runID), fromSeq)
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return meta, nil, err
		}
		meta = out.Meta
		entries = append(entries, out.Entries...)
		if out.NextSeq == 0 {
			return meta, entries, nil
		}
		fromSeq = out.NextSeq
	}
}

// JournalArtifactUpload calls POST /v0/journal/{run_id}/artifacts?name=&kind=
// with the raw artifact bytes and returns the stored artifact ref. The run's
// lease must still be active — finished runs are immutable evidence.
func (c *Client) JournalArtifactUpload(ctx context.Context, runID, name, kind string, data io.Reader) (journal.ArtifactRef, error) {
	var out struct {
		Artifact journal.ArtifactRef `json:"artifact"`
	}
	q := url.Values{"name": {name}}
	if kind != "" {
		q.Set("kind", kind)
	}
	path := fmt.Sprintf("/v0/journal/%s/artifacts?%s", url.PathEscape(runID), q.Encode())
	err := c.doReader(ctx, http.MethodPost, path, "application/octet-stream", data, &out)
	return out.Artifact, err
}
