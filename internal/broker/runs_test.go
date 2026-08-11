package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

func postRun(t *testing.T, h http.Handler, req proto.RunRequest) (int, proto.Run) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(mustJSON(t, req)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var run proto.Run
	if rec.Code == http.StatusOK || rec.Code == http.StatusAccepted {
		if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
			t.Fatalf("decode run: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, run
}

func TestFederatedRunSync(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemon(t, sim("BBBB-1", "iOS 18.5", "iPhone 16"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})
	h := b.Handler()

	// ios18 only exists on host two: placement must land there.
	code, run := postRun(t, h, proto.RunRequest{
		AgentID: "qa",
		Spec: proto.RunSpec{
			Name:   "sync-run",
			Target: proto.RunTarget{Labels: []string{"ios18"}},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("sync run: code=%d", code)
	}
	if run.State != proto.RunPassed {
		t.Fatalf("run state=%s error=%v", run.State, run.Error)
	}
	if run.Host != "two" || run.HostAddr != d2.srv.URL {
		t.Fatalf("run host annotation: host=%q addr=%q", run.Host, run.HostAddr)
	}
	if run.TargetUDID != "BBBB-1" {
		t.Fatalf("run target: %q", run.TargetUDID)
	}

	// GET /v0/runs/{id} routes to the owning daemon.
	rec, _ := doJSON(t, h, http.MethodGet, "/v0/runs/"+run.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get run: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// The federated list carries the run, annotated.
	recList := httptest.NewRecorder()
	h.ServeHTTP(recList, httptest.NewRequest(http.MethodGet, "/v0/runs", nil))
	var list proto.RunList
	if err := json.Unmarshal(recList.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range list.Runs {
		if r.ID == run.ID && r.Host == "two" {
			found = true
		}
	}
	if !found {
		t.Fatalf("run %s not in federated list: %s", run.ID, recList.Body.String())
	}
}

func TestFederatedRunAsyncAndHostPin(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	d2 := newFakeDaemon(t, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{
		{Name: "one", Addr: d1.srv.URL},
		{Name: "two", Addr: d2.srv.URL},
	})
	h := b.Handler()

	// Pin to host "two" via a host-level label; async answers 202.
	code, run := postRun(t, h, proto.RunRequest{
		AgentID: "qa", Async: true,
		Spec: proto.RunSpec{
			Target: proto.RunTarget{Labels: []string{"two", "ios26"}},
		},
	})
	if code != http.StatusAccepted {
		t.Fatalf("async run: code=%d", code)
	}
	if run.Host != "two" {
		t.Fatalf("pinned run placed on %q", run.Host)
	}

	// Poll through the broker until terminal.
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, _ := doJSON(t, h, http.MethodGet, "/v0/runs/"+run.ID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("poll run: code=%d body=%s", rec.Code, rec.Body.String())
		}
		var cur proto.Run
		if err := json.Unmarshal(rec.Body.Bytes(), &cur); err != nil {
			t.Fatal(err)
		}
		if cur.State == proto.RunPassed {
			if cur.TargetUDID != "BBBB-1" {
				t.Fatalf("pinned run target: %q", cur.TargetUDID)
			}
			break
		}
		if cur.State == proto.RunFailed {
			t.Fatalf("run failed: %v", cur.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not finish: %s", cur.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestFederatedRunErrors(t *testing.T) {
	d1 := newFakeDaemon(t, sim("AAAA-1", "iOS 26.5", "iPhone 17 Pro"))
	b := newTestBroker(t, []HostConfig{{Name: "one", Addr: d1.srv.URL}})
	h := b.Handler()

	// No matching host → 409 no_match.
	rec, out := doJSON(t, h, http.MethodPost, "/v0/runs", proto.RunRequest{
		AgentID: "qa",
		Spec:    proto.RunSpec{Target: proto.RunTarget{Labels: []string{"nope"}}},
	})
	if rec.Code != http.StatusConflict || out["code"] != proto.ErrNoMatch {
		t.Fatalf("no match: code=%d body=%v", rec.Code, out)
	}

	// Missing agent_id → 400 before any placement.
	rec, out = doJSON(t, h, http.MethodPost, "/v0/runs", proto.RunRequest{
		Spec: proto.RunSpec{Target: proto.RunTarget{Labels: []string{"ios26"}}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing agent: code=%d body=%v", rec.Code, out)
	}

	// A host-independent spec error (reserved target.image) is forwarded
	// from the daemon verbatim: 501 not_implemented.
	rec, out = doJSON(t, h, http.MethodPost, "/v0/runs", proto.RunRequest{
		AgentID: "qa",
		Spec:    proto.RunSpec{Target: proto.RunTarget{Labels: []string{"ios26"}, Image: "golden"}},
	})
	if rec.Code != http.StatusNotImplemented || out["code"] != proto.ErrNotImplemented {
		t.Fatalf("reserved image: code=%d body=%v", rec.Code, out)
	}

	// A pure host pin has no target selector left after host labels are
	// stripped → clear 400 instead of the daemon's generic one.
	rec, out = doJSON(t, h, http.MethodPost, "/v0/runs", proto.RunRequest{
		AgentID: "qa",
		Spec:    proto.RunSpec{Target: proto.RunTarget{Labels: []string{"one"}}},
	})
	if rec.Code != http.StatusBadRequest || out["code"] != proto.ErrBadRequest {
		t.Fatalf("pure host pin: code=%d body=%v", rec.Code, out)
	}
	if msg, _ := out["message"].(string); !strings.Contains(msg, "pinned to host") {
		t.Fatalf("pure host pin message: %v", out)
	}

	// A label that is host-extra on one host but target-derived on
	// another must not abort placement: the host whose strip empties the
	// selector set is skipped and the run lands on the other host.
	d2 := newFakeDaemon(t, sim("BBBB-1", "iOS 26.5", "iPhone 17 Pro"))
	d3 := newFakeDaemon(t, sim("CCCC-1", "iOS 18.0", "iPhone 15"))
	b2 := New(Config{Hosts: []HostConfig{
		{Name: "pinned", Addr: d3.srv.URL, Labels: []string{"ios26"}},
		{Name: "real", Addr: d2.srv.URL},
	}}, slog.Default(), Options{ProbeInterval: time.Hour})
	b2.probeAll(context.Background())
	rec, out = doJSON(t, b2.Handler(), http.MethodPost, "/v0/runs", proto.RunRequest{
		AgentID: "qa",
		Async:   true,
		Spec: proto.RunSpec{
			Target: proto.RunTarget{Labels: []string{"ios26"}},
		},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("host-extra label skip: code=%d body=%v", rec.Code, out)
	}
	if out["host"] != "real" {
		t.Fatalf("host-extra label skip placed on %v, want real", out["host"])
	}

	// A burst of runs within one probe interval spreads across hosts:
	// each accepted run bumps its host's placement load immediately, so
	// the second run ranks the other (now less loaded) host first.
	d4 := newFakeDaemon(t, sim("DDDD-1", "iOS 26.5", "iPhone 17 Pro"))
	d5 := newFakeDaemon(t, sim("EEEE-1", "iOS 26.5", "iPhone 17 Pro"))
	b3 := New(Config{Hosts: []HostConfig{
		{Name: "h1", Addr: d4.srv.URL},
		{Name: "h2", Addr: d5.srv.URL},
	}}, slog.Default(), Options{ProbeInterval: time.Hour})
	b3.probeAll(context.Background())
	hosts := map[string]int{}
	for i := 0; i < 2; i++ {
		rec, out = doJSON(t, b3.Handler(), http.MethodPost, "/v0/runs", proto.RunRequest{
			AgentID: "qa",
			Async:   true,
			Spec:    proto.RunSpec{Target: proto.RunTarget{Labels: []string{"ios26"}}},
		})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("burst run %d: code=%d body=%v", i, rec.Code, out)
		}
		hosts[out["host"].(string)]++
	}
	if len(hosts) != 2 {
		t.Fatalf("burst runs herded onto one host: %v", hosts)
	}

	// The probe round settles loaded runs that finished without the
	// broker observing it (async runs never polled), so placement load
	// doesn't leak until the routing entry is pruned.
	deadline := time.Now().Add(10 * time.Second)
	for {
		b3.probeAll(context.Background())
		n := 0
		for _, bh := range b3.hosts {
			n += len(b3.loadedRuns(bh))
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("finished async runs still loaded after reconcile: %d", n)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Unknown run ID → 404.
	rec, out = doJSON(t, h, http.MethodGet, "/v0/runs/run_missing", nil)
	if rec.Code != http.StatusNotFound || out["code"] != proto.ErrNotFound {
		t.Fatalf("unknown run: code=%d body=%v", rec.Code, out)
	}
}
