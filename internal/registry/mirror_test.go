package registry

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
	"github.com/BariBariGood/manzanas/proto"
)

// fakeProbe scripts mirrord statuses per socket and counts probes.
type fakeProbe struct {
	statuses map[string]mirror.Status
	errs     map[string]error
	calls    int
}

func (p *fakeProbe) probe(ctx context.Context, socket string) (mirror.Status, error) {
	p.calls++
	if err := p.errs[socket]; err != nil {
		return mirror.Status{}, err
	}
	return p.statuses[socket], nil
}

func mirrorFixture(p *fakeProbe) *MirrorRegistry {
	r := NewMirror(p.probe)
	r.SetMirrors(map[string]string{"MIRROR-1": "/tmp/m.sock"})
	return r
}

func TestMirrorReadyMapsToBooted(t *testing.T) {
	p := &fakeProbe{statuses: map[string]mirror.Status{"/tmp/m.sock": {State: "ready"}}}
	r := mirrorFixture(p)
	tg, err := r.Get(context.Background(), "MIRROR-1")
	if err != nil {
		t.Fatal(err)
	}
	if tg.State != proto.StateBooted || tg.Kind != proto.TargetDevice || tg.Name != "iPhone (mirror)" {
		t.Fatalf("target = %+v", tg)
	}
	for _, want := range []string{"device", MirrorLabel, NoDevicectlLabel} {
		if !slices.Contains(tg.Labels, want) {
			t.Fatalf("labels missing %q: %v", want, tg.Labels)
		}
	}
	if slices.Contains(tg.Labels, DisconnectedLabel) || slices.Contains(tg.Labels, MirrorBlockedLabel) {
		t.Fatalf("ready target carries failure labels: %v", tg.Labels)
	}
}

func TestMirrorBlockedStaysBootedWithLabel(t *testing.T) {
	p := &fakeProbe{statuses: map[string]mirror.Status{"/tmp/m.sock": {State: "blocked", Marker: "iPhone in Use"}}}
	tg, err := mirrorFixture(p).Get(context.Background(), "MIRROR-1")
	if err != nil {
		t.Fatal(err)
	}
	if tg.State != proto.StateBooted || !slices.Contains(tg.Labels, MirrorBlockedLabel) {
		t.Fatalf("target = %+v", tg)
	}
}

func TestMirrorDownMapsToUnknownDisconnected(t *testing.T) {
	for name, p := range map[string]*fakeProbe{
		"not-running": {statuses: map[string]mirror.Status{"/tmp/m.sock": {State: "not-running"}}},
		"no-window":   {statuses: map[string]mirror.Status{"/tmp/m.sock": {State: "no-window"}}},
		"unreachable": {errs: map[string]error{"/tmp/m.sock": errors.New("dial unix: no such file")}},
	} {
		tg, err := mirrorFixture(p).Get(context.Background(), "MIRROR-1")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tg.State != proto.StateUnknown || !slices.Contains(tg.Labels, DisconnectedLabel) {
			t.Fatalf("%s: target = %+v", name, tg)
		}
	}
}

func TestMirrorStatusCacheTTL(t *testing.T) {
	p := &fakeProbe{statuses: map[string]mirror.Status{"/tmp/m.sock": {State: "ready"}}}
	r := mirrorFixture(p)
	now := time.Unix(1000, 0)
	r.now = func() time.Time { return now }
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := r.Get(ctx, "MIRROR-1"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.List(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if p.calls != 1 {
		t.Fatalf("probe called %d times within TTL, want 1", p.calls)
	}
	now = now.Add(mirrorStatusTTL + time.Second)
	if _, err := r.Get(ctx, "MIRROR-1"); err != nil {
		t.Fatal(err)
	}
	if p.calls != 2 {
		t.Fatalf("probe called %d times after TTL expiry, want 2", p.calls)
	}
}

func TestMirrorSetMirrorsReplacesAndInvalidates(t *testing.T) {
	p := &fakeProbe{statuses: map[string]mirror.Status{
		"/tmp/m.sock": {State: "ready"},
		"/tmp/other":  {State: "not-running"},
	}}
	r := mirrorFixture(p)
	ctx := context.Background()
	if tg, _ := r.Get(ctx, "MIRROR-1"); tg.State != proto.StateBooted {
		t.Fatalf("target = %+v", tg)
	}
	// Re-socketing the same UDID drops the cached status.
	r.SetMirrors(map[string]string{"MIRROR-1": "/tmp/other"})
	if tg, _ := r.Get(ctx, "MIRROR-1"); tg.State != proto.StateUnknown {
		t.Fatalf("stale cache survived a socket change: %+v", tg)
	}
	// Removing the UDID removes the target.
	r.SetMirrors(map[string]string{})
	var nf *NotFoundError
	if _, err := r.Get(ctx, "MIRROR-1"); !errors.As(err, &nf) {
		t.Fatalf("got %v, want NotFoundError", err)
	}
	if ts, err := r.List(ctx); err != nil || len(ts) != 0 {
		t.Fatalf("List = %+v, %v", ts, err)
	}
}

func TestMirrorBootShutdownRefused(t *testing.T) {
	p := &fakeProbe{statuses: map[string]mirror.Status{"/tmp/m.sock": {State: "ready"}}}
	r := mirrorFixture(p)
	ctx := context.Background()
	var be *DeviceBootError
	if err := r.Boot(ctx, "MIRROR-1"); !errors.As(err, &be) {
		t.Fatalf("boot: got %v, want DeviceBootError", err)
	}
	var se *DeviceShutdownError
	if err := r.Shutdown(ctx, "MIRROR-1"); !errors.As(err, &se) {
		t.Fatalf("shutdown: got %v, want DeviceShutdownError", err)
	}
	var nf *NotFoundError
	if err := r.Boot(ctx, "nope"); !errors.As(err, &nf) {
		t.Fatalf("boot unknown: got %v, want NotFoundError", err)
	}
}

func TestMirrorHealth(t *testing.T) {
	p := &fakeProbe{statuses: map[string]mirror.Status{"/tmp/m.sock": {State: "ready"}}}
	r := mirrorFixture(p)
	if st, err := r.Health(context.Background(), "MIRROR-1"); err != nil || st != proto.StateBooted {
		t.Fatalf("health = %q, %v", st, err)
	}
}

func TestMirrorCallerCancellationNotCached(t *testing.T) {
	p := &fakeProbe{statuses: map[string]mirror.Status{"/tmp/m.sock": {State: "ready"}}}
	r := mirrorFixture(p)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The probe itself would succeed, but simulate the usual outcome of a
	// dead caller context: the probe fails with the context error.
	r.probe = func(pctx context.Context, socket string) (mirror.Status, error) {
		p.calls++
		return mirror.Status{}, ctx.Err()
	}
	if tg, err := r.Get(ctx, "MIRROR-1"); err != nil || tg.State != proto.StateUnknown {
		t.Fatalf("Get = %+v, %v", tg, err)
	}
	// A fresh caller must not inherit the cancelled probe's failure.
	r.probe = p.probe
	tg, err := r.Get(context.Background(), "MIRROR-1")
	if err != nil {
		t.Fatal(err)
	}
	if tg.State != proto.StateBooted {
		t.Fatalf("cancelled probe poisoned the cache: %+v", tg)
	}
}

// fakeMirrordSocket serves a mirrord-shaped /v1/status on a unix socket.
// oldHelper mimics a helper predating the light mode: any query string
// falls off the route and answers the 404 bad-request route error.
func fakeMirrordSocket(t *testing.T, oldHelper bool) (string, *mirrordCalls) {
	t.Helper()
	calls := &mirrordCalls{state: "ready", old: oldHelper}
	// Not t.TempDir(): its per-test-name path blows past macOS's 104-byte
	// sun_path limit for unix sockets.
	dir, err := os.MkdirTemp("", "m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	startFakeMirrord(t, sock, calls)
	return sock, calls
}

func startFakeMirrord(t *testing.T, sock string, calls *mirrordCalls) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		light := r.URL.Query().Get("light") == "1"
		w.Header().Set("Content-Type", "application/json")
		if calls.oldHelper() && r.URL.RawQuery != "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"bad-request","error":"no route GET ` + r.URL.String() + `"}`))
			return
		}
		calls.record(light)
		state := calls.fullState()
		if light && state == "blocked" {
			// Light mode cannot see the blocked interstitial.
			state = "ready"
		}
		out := map[string]any{"state": state, "frontmost": true}
		if light {
			out["probe"] = "light"
		}
		_ = json.NewEncoder(w).Encode(out)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

type mirrordCalls struct {
	mu          sync.Mutex
	light, full int
	state       string
	old         bool
}

func (c *mirrordCalls) setOld(old bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.old = old
}

func (c *mirrordCalls) oldHelper() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.old
}

func (c *mirrordCalls) setState(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = s
}

func (c *mirrordCalls) fullState() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *mirrordCalls) record(light bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if light {
		c.light++
	} else {
		c.full++
	}
}

func (c *mirrordCalls) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.light, c.full
}

func TestDefaultProbeUsesLightStatus(t *testing.T) {
	sock, calls := fakeMirrordSocket(t, false)
	now := time.Unix(1000, 0)
	probe := newDefaultMirrorProbe(func() time.Time { return now })
	ctx := context.Background()

	// First probe: full (blocked detection at startup). In-interval
	// probes: light. After the interval: full again.
	for i, want := range []struct{ light, full int }{
		{0, 1}, {1, 1}, {2, 1},
	} {
		st, err := probe(ctx, sock)
		if err != nil || st.State != "ready" {
			t.Fatalf("probe %d: %+v, %v", i, st, err)
		}
		if light, full := calls.counts(); light != want.light || full != want.full {
			t.Fatalf("probe %d: calls = %d light, %d full; want %+v", i, light, full, want)
		}
		now = now.Add(10 * time.Second)
	}
	now = now.Add(mirrorFullStatusInterval)
	if _, err := probe(ctx, sock); err != nil {
		t.Fatal(err)
	}
	if light, full := calls.counts(); light != 2 || full != 2 {
		t.Fatalf("calls = %d light, %d full; want a second full after the interval", light, full)
	}
}

func TestDefaultProbeStaysFullWhileBlocked(t *testing.T) {
	sock, calls := fakeMirrordSocket(t, false)
	calls.setState("blocked")
	now := time.Unix(1000, 0)
	probe := newDefaultMirrorProbe(func() time.Time { return now })
	ctx := context.Background()

	st, err := probe(ctx, sock)
	if err != nil || st.State != "blocked" {
		t.Fatalf("status = %+v, %v", st, err)
	}
	// While blocked, probes stay on the full status so the label holds
	// steady instead of flickering with the cache TTL.
	now = now.Add(10 * time.Second)
	if st, err = probe(ctx, sock); err != nil || st.State != "blocked" {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if light, full := calls.counts(); light != 0 || full != 2 {
		t.Fatalf("calls = %d light, %d full; want full only while blocked", light, full)
	}
	// Once the interstitial clears, probes go light again.
	calls.setState("ready")
	now = now.Add(10 * time.Second)
	if st, err = probe(ctx, sock); err != nil || st.State != "ready" {
		t.Fatalf("status = %+v, %v", st, err)
	}
	now = now.Add(10 * time.Second)
	if _, err = probe(ctx, sock); err != nil {
		t.Fatal(err)
	}
	if light, full := calls.counts(); light != 1 || full != 3 {
		t.Fatalf("calls = %d light, %d full; want light again after unblocking", light, full)
	}
}

func TestDefaultProbeFallsBackOnOldHelper(t *testing.T) {
	sock, calls := fakeMirrordSocket(t, true)
	now := time.Unix(1000, 0)
	probe := newDefaultMirrorProbe(func() time.Time { return now })
	ctx := context.Background()

	if st, err := probe(ctx, sock); err != nil || st.State != "ready" {
		t.Fatalf("status = %+v, %v", st, err)
	}
	// Within the interval a light probe is attempted, 404s (not counted:
	// the fake rejects the query path before recording), and falls back
	// to the full status; the unsupported answer is remembered so the
	// probe after that goes straight to the full status.
	now = now.Add(10 * time.Second)
	if _, err := probe(ctx, sock); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	if _, err := probe(ctx, sock); err != nil {
		t.Fatal(err)
	}
	if light, full := calls.counts(); light != 0 || full != 3 {
		t.Fatalf("calls = %d light, %d full; want 3 full, no light", light, full)
	}
	// The light route is re-tried once per interval, so an in-place
	// helper upgrade is picked up without a daemon restart.
	calls.setOld(false)
	now = now.Add(mirrorFullStatusInterval - 10*time.Second)
	if st, err := probe(ctx, sock); err != nil || st.State != "ready" {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if light, full := calls.counts(); light != 1 || full != 3 {
		t.Fatalf("calls = %d light, %d full; want light retried after upgrade", light, full)
	}
}

func TestDefaultProbeFailedFullDoesNotStickFull(t *testing.T) {
	// An unreachable helper fails the (first, full) probe; once it comes
	// back, in-interval probes go light rather than staying on the
	// expensive path forever.
	dir, err := os.MkdirTemp("", "m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	now := time.Unix(1000, 0)
	probe := newDefaultMirrorProbe(func() time.Time { return now })
	ctx := context.Background()

	if _, err := probe(ctx, sock); err == nil {
		t.Fatal("probe of a dead socket succeeded")
	}

	calls := &mirrordCalls{state: "ready"}
	startFakeMirrord(t, sock, calls)
	now = now.Add(10 * time.Second)
	if st, err := probe(ctx, sock); err != nil || st.State != "ready" {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if light, full := calls.counts(); light != 1 || full != 0 {
		t.Fatalf("calls = %d light, %d full; want light after failed full", light, full)
	}
}
