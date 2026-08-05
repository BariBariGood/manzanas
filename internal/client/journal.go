package client

import (
	"context"
	"fmt"
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
