package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

func newJournalTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	store, err := journal.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Wired before lease.New so the expiry goroutine never races SetJournal.
	srv.SetJournal(journal.NewRecorder(store))
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestJournalNotWiredReturns501(t *testing.T) {
	ts := newTestServer(t) // no journal wired
	resp := doJSON(t, "GET", ts.URL+"/v0/journal", nil, nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

func TestJournalRecordsLeaseLifecycle(t *testing.T) {
	ts := newJournalTestServer(t)

	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "journal-test"}, &l)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("acquire: %d", resp.StatusCode)
	}

	// Boot then release, all journaled under the lease's run.
	resp = doJSON(t, "POST", ts.URL+"/v0/targets/"+l.TargetUDID+"/boot",
		map[string]any{"lease_id": l.ID}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("boot: %d", resp.StatusCode)
	}

	// Upload an artifact (screenshot round-trip).
	req, _ := http.NewRequest("POST",
		ts.URL+"/v0/journal/"+l.ID+"/artifacts?name=shot.png", strings.NewReader("png-bytes"))
	aresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var put struct {
		Artifact journal.ArtifactRef `json:"artifact"`
	}
	if err := json.NewDecoder(aresp.Body).Decode(&put); err != nil {
		t.Fatal(err)
	}
	aresp.Body.Close()
	if aresp.StatusCode != http.StatusCreated || put.Artifact.SHA256 == "" {
		t.Fatalf("artifact put: %d %+v", aresp.StatusCode, put)
	}

	resp = doJSON(t, "DELETE", ts.URL+"/v0/leases/"+l.ID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release: %d", resp.StatusCode)
	}

	// Fetch the journal and verify the recorded actions.
	var page struct {
		Meta    journal.RunMeta `json:"meta"`
		Entries []journal.Entry `json:"entries"`
		NextSeq int64           `json:"next_seq"`
	}
	resp = doJSON(t, "GET", ts.URL+"/v0/journal/"+l.ID, nil, &page)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("journal get: %d", resp.StatusCode)
	}
	if page.Meta.TargetUDID != l.TargetUDID {
		t.Fatalf("meta = %+v", page.Meta)
	}
	actions := map[string]bool{}
	for _, e := range page.Entries {
		if a, ok := e.Payload["action"].(string); ok {
			actions[a] = true
		}
	}
	for _, want := range []string{"leases.acquire", "targets.boot", "journal.artifact", "leases.release"} {
		if !actions[want] {
			t.Errorf("missing journaled action %q (got %v)", want, actions)
		}
	}

	// Artifact round-trips over the API, using the run-relative path from
	// artifacts[].path as documented (and the bare name for good measure).
	for _, p := range []string{put.Artifact.Path, strings.TrimPrefix(put.Artifact.Path, "artifacts/")} {
		get, err := http.Get(ts.URL + "/v0/journal/" + l.ID + "/artifacts/" + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(get.Body)
		get.Body.Close()
		if get.StatusCode != http.StatusOK || string(body) != "png-bytes" {
			t.Fatalf("artifact fetch %q: status=%d body=%q", p, get.StatusCode, body)
		}
	}

	// Markdown export renders.
	md, err := http.Get(ts.URL + "/v0/journal/" + l.ID + "/export.md")
	if err != nil {
		t.Fatal(err)
	}
	mdBody, _ := io.ReadAll(md.Body)
	md.Body.Close()
	if md.StatusCode != http.StatusOK || !strings.Contains(string(mdBody), "targets.boot") {
		t.Fatalf("export: %d %q", md.StatusCode, mdBody)
	}
}

func TestLeaseLifecycleMarksRunOpenForGC(t *testing.T) {
	reg := registry.NewMock()
	srv := New(reg, nil, nil)
	store, err := journal.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv.SetJournal(journal.NewRecorder(store))
	leases := lease.New(reg, srv.EventSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "gc-test"}, &l)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("acquire: %d", resp.StatusCode)
	}

	// While the lease is live, an aggressive GC must not reclaim its run.
	removed, err := store.GC(journal.GCConfig{MaxBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range removed {
		if id == l.ID {
			t.Fatalf("GC reclaimed run for active lease %s", l.ID)
		}
	}

	resp = doJSON(t, "DELETE", ts.URL+"/v0/leases/"+l.ID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release: %d", resp.StatusCode)
	}

	// After release the run is eligible again.
	removed, err = store.GC(journal.GCConfig{MaxBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range removed {
		found = found || id == l.ID
	}
	if !found {
		t.Fatalf("GC did not reclaim released run %s (removed %v)", l.ID, removed)
	}
}

func TestJournalPaginationOverAPI(t *testing.T) {
	ts := newJournalTestServer(t)
	var l proto.Lease
	doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"ios26"}, AgentID: "pager"}, &l)
	for i := 0; i < 3; i++ {
		doJSON(t, "POST", ts.URL+"/v0/leases/"+l.ID+"/renew", proto.RenewLeaseRequest{}, nil)
	}
	var page struct {
		Entries []journal.Entry `json:"entries"`
		NextSeq int64           `json:"next_seq"`
	}
	resp := doJSON(t, "GET", ts.URL+"/v0/journal/"+l.ID+"?limit=2", nil, &page)
	if resp.StatusCode != http.StatusOK || len(page.Entries) != 2 || page.NextSeq != 3 {
		t.Fatalf("page1: %d %+v", resp.StatusCode, page)
	}
	resp = doJSON(t, "GET", ts.URL+"/v0/journal/"+l.ID+"?from_seq=3&limit=100", nil, &page)
	if resp.StatusCode != http.StatusOK || len(page.Entries) != 2 || page.NextSeq != 0 {
		t.Fatalf("page2: %d %+v", resp.StatusCode, page)
	}
}
