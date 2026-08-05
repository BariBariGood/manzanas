// manzanas-bench measures the daemon's hot-path latencies against a live
// manzanasd: warm-pool thaw (lease grant on a parked sim), plain lease
// grant, cold boot, and per-action latency (tap / observe). It is a pure
// protocol client — point it at any daemon and it prints per-sample
// latencies plus a percentile summary, so the README/blog numbers can be
// reproduced on any Mac with `make bench`.
//
// Usage:
//
//	manzanas-bench --daemon http://localhost:7433 --udid <UDID> \
//	    --phase thaw --samples 20
//
// Phases:
//
//	thaw     lease grant on a parked warm-pool sim (includes the SIGCONT
//	         thaw). Requires the target to be a pool member; each sample
//	         waits for the sim to be re-parked after release.
//	grant    lease grant on a booted, un-parked sim (the protocol/bookkeeping
//	         baseline to subtract from thaw).
//	boot     shutdown + boot cycles under one lease; reports the boot leg.
//	tap      POST /v0/actions {"kind":"tap"} under one lease.
//	observe  POST /v0/actions {"kind":"observe"} under one lease.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

type client struct {
	base string
	http *http.Client
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *client) target(ctx context.Context, udid string) (*proto.Target, error) {
	var out struct {
		Targets []proto.Target `json:"targets"`
	}
	if err := c.do(ctx, http.MethodGet, "/v0/targets", nil, &out); err != nil {
		return nil, err
	}
	for i := range out.Targets {
		if out.Targets[i].UDID == udid {
			return &out.Targets[i], nil
		}
	}
	return nil, fmt.Errorf("target %s not in daemon target list", udid)
}

func (c *client) acquire(ctx context.Context, udid, agent string, ttl int) (*proto.Lease, error) {
	var lease proto.Lease
	req := proto.AcquireLeaseRequest{UDID: udid, AgentID: agent, TTLSeconds: ttl, Purpose: "bench"}
	if err := c.do(ctx, http.MethodPost, "/v0/leases", req, &lease); err != nil {
		return nil, err
	}
	for lease.State == proto.LeaseQueued {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		if err := c.do(ctx, http.MethodGet, "/v0/leases/"+url.PathEscape(lease.ID), nil, &lease); err != nil {
			return nil, err
		}
	}
	if lease.State != proto.LeaseActive {
		return nil, fmt.Errorf("lease %s in unexpected state %q", lease.ID, lease.State)
	}
	return &lease, nil
}

func (c *client) release(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v0/leases/"+url.PathEscape(id), nil, nil)
}

func (c *client) waitState(ctx context.Context, udid string, want proto.TargetState, poll time.Duration) error {
	for {
		t, err := c.target(ctx, udid)
		if err != nil {
			return err
		}
		if t.State == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s to reach %s (currently %s): %w", udid, want, t.State, ctx.Err())
		case <-time.After(poll):
		}
	}
}

func (c *client) waitParked(ctx context.Context, udid string) error {
	for {
		t, err := c.target(ctx, udid)
		if err != nil {
			return err
		}
		if t.Warm {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s to be re-parked: %w", udid, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func summarize(name string, ms []float64) map[string]any {
	sorted := append([]float64(nil), ms...)
	sort.Float64s(sorted)
	pct := func(p float64) float64 {
		if len(sorted) == 0 {
			return 0
		}
		i := int(p*float64(len(sorted))+0.5) - 1
		if i < 0 {
			i = 0
		}
		if i >= len(sorted) {
			i = len(sorted) - 1
		}
		return sorted[i]
	}
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	if len(sorted) == 0 {
		fmt.Printf("%s: no samples\n", name)
		return map[string]any{"phase": name, "n": 0}
	}
	mean := sum / float64(len(sorted))
	s := map[string]any{
		"phase": name, "n": len(sorted),
		"min": sorted[0], "p50": pct(0.50), "p90": pct(0.90), "p95": pct(0.95),
		"max": sorted[len(sorted)-1], "mean": mean,
	}
	fmt.Printf("%s: n=%d min=%.1fms p50=%.1fms p90=%.1fms p95=%.1fms max=%.1fms mean=%.1fms\n",
		name, len(sorted), sorted[0], pct(0.50), pct(0.90), pct(0.95), sorted[len(sorted)-1], mean)
	return s
}

func main() {
	daemon := flag.String("daemon", "http://localhost:7433", "daemon base URL")
	udid := flag.String("udid", "", "target UDID (required)")
	phase := flag.String("phase", "", "thaw | grant | boot | tap | observe")
	samples := flag.Int("samples", 20, "number of samples")
	agent := flag.String("agent", "manzanas-bench", "lease agent_id")
	jsonOut := flag.String("json", "", "append the summary as a JSON line to this file")
	label := flag.String("label", "", "label for the printed/JSON summary (defaults to the phase name)")
	timeout := flag.Duration("timeout", 30*time.Minute, "overall budget")
	flag.Parse()
	if *udid == "" || *phase == "" {
		fmt.Fprintln(os.Stderr, "usage: manzanas-bench --udid <UDID> --phase thaw|grant|boot|tap|observe [--samples N]")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	c := &client{base: strings.TrimRight(*daemon, "/"), http: &http.Client{}}

	var ms []float64
	record := func(d time.Duration) {
		v := float64(d.Microseconds()) / 1000.0
		ms = append(ms, v)
		fmt.Printf("  sample %d: %.1f ms\n", len(ms), v)
	}

	fail := func(err error) {
		fmt.Fprintf(os.Stderr, "bench %s: %v\n", *phase, err)
		os.Exit(1)
	}

	switch *phase {
	case "thaw", "grant":
		for i := 0; i < *samples; i++ {
			if *phase == "thaw" {
				if err := c.waitParked(ctx, *udid); err != nil {
					fail(err)
				}
			}
			start := time.Now()
			lease, err := c.acquire(ctx, *udid, *agent, 120)
			if err != nil {
				fail(err)
			}
			record(time.Since(start))
			if err := c.release(ctx, lease.ID); err != nil {
				fail(err)
			}
		}
	case "boot":
		lease, err := c.acquire(ctx, *udid, *agent, 1800)
		if err != nil {
			fail(err)
		}
		defer c.release(context.Background(), lease.ID)
		body := map[string]string{"lease_id": lease.ID}
		for i := 0; i < *samples; i++ {
			if err := c.do(ctx, http.MethodPost, "/v0/targets/"+url.PathEscape(*udid)+"/shutdown", body, nil); err != nil {
				fail(err)
			}
			if err := c.waitState(ctx, *udid, proto.StateShutdown, 250*time.Millisecond); err != nil {
				fail(err)
			}
			time.Sleep(2 * time.Second) // let CoreSimulator settle between cycles
			start := time.Now()
			if err := c.do(ctx, http.MethodPost, "/v0/targets/"+url.PathEscape(*udid)+"/boot", body, nil); err != nil {
				fail(err)
			}
			if err := c.waitState(ctx, *udid, proto.StateBooted, 100*time.Millisecond); err != nil {
				fail(err)
			}
			record(time.Since(start))
		}
		if err := c.release(ctx, lease.ID); err != nil {
			fail(err)
		}
	case "tap", "observe":
		lease, err := c.acquire(ctx, *udid, *agent, 1800)
		if err != nil {
			fail(err)
		}
		defer c.release(context.Background(), lease.ID)
		body := map[string]string{"lease_id": lease.ID}
		if err := c.do(ctx, http.MethodPost, "/v0/targets/"+url.PathEscape(*udid)+"/boot", body, nil); err != nil {
			fail(err)
		}
		if err := c.waitState(ctx, *udid, proto.StateBooted, 250*time.Millisecond); err != nil {
			fail(err)
		}
		payload := map[string]any{"x": 200, "y": 400}
		if *phase == "observe" {
			payload = nil
		}
		// One unrecorded warmup action: the first warm action pays the
		// resident-helper spawn; the first cold action pays simulator-side
		// caches. Reported separately so the steady state is honest.
		warmup := time.Now()
		if _, err := c.action(ctx, lease.ID, *phase, payload); err != nil {
			fail(err)
		}
		fmt.Printf("  warmup (first action, excluded): %.1f ms\n", float64(time.Since(warmup).Microseconds())/1000.0)
		for i := 0; i < *samples; i++ {
			start := time.Now()
			if _, err := c.action(ctx, lease.ID, *phase, payload); err != nil {
				fail(err)
			}
			record(time.Since(start))
		}
		if err := c.release(ctx, lease.ID); err != nil {
			fail(err)
		}
	default:
		fail(fmt.Errorf("unknown phase %q", *phase))
	}

	name := *phase
	if *label != "" {
		name = *label
	}
	s := summarize(name, ms)
	if *jsonOut != "" {
		f, err := os.OpenFile(*jsonOut, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fail(err)
		}
		defer f.Close()
		raw, _ := json.Marshal(s)
		fmt.Fprintln(f, string(raw))
	}
}

func (c *client) action(ctx context.Context, leaseID, kind string, payload map[string]any) (*proto.ActionResult, error) {
	var res proto.ActionResult
	req := proto.ActionRequest{LeaseID: leaseID, Kind: kind, Payload: payload}
	if err := c.do(ctx, http.MethodPost, "/v0/actions", req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
