package warm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// fakeHost is an in-memory Host: a mutable process table plus recorded
// signals and settable gauges.
type fakeHost struct {
	mu      sync.Mutex
	procs   []Proc
	signals map[int][]syscall.Signal
	stopped map[int]bool
	load    float64
	cpus    int
	free    uint64
	sigErr  map[int]error
	fpErr   error
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		signals: make(map[int][]syscall.Signal),
		stopped: make(map[int]bool),
		cpus:    8,
		load:    1.0,
		free:    100 << 30,
		sigErr:  make(map[int]error),
	}
}

func (h *fakeHost) setSimTree(udid string, rootPID int, childPIDs ...int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.procs = append(h.procs, Proc{PID: rootPID, PPID: 1, RSSKB: 1000,
		Command: fmt.Sprintf("/usr/libexec/launchd_sim .../Devices/%s/data/var/run/launchd_bootstrap.plist", udid)})
	for _, c := range childPIDs {
		h.procs = append(h.procs, Proc{PID: c, PPID: rootPID, RSSKB: 1000, Command: "simdaemon"})
	}
}

func (h *fakeHost) removePID(pid int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.procs[:0]
	for _, p := range h.procs {
		if p.PID != pid {
			out = append(out, p)
		}
	}
	h.procs = out
}

func (h *fakeHost) Processes(context.Context) ([]Proc, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := append([]Proc(nil), h.procs...)
	for i := range out {
		if h.stopped[out[i].PID] {
			out[i].State = "T"
		} else if out[i].State == "" {
			out[i].State = "S"
		}
	}
	return out, nil
}

func (h *fakeHost) Signal(pid int, sig syscall.Signal) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.sigErr[pid]; err != nil {
		return err
	}
	h.signals[pid] = append(h.signals[pid], sig)
	switch sig {
	case syscall.SIGSTOP:
		h.stopped[pid] = true
	case syscall.SIGCONT:
		h.stopped[pid] = false
	}
	return nil
}

func (h *fakeHost) LoadAvg1() (float64, error)           { return h.load, nil }
func (h *fakeHost) NumCPU() int                          { return h.cpus }
func (h *fakeHost) FreeDiskBytes(string) (uint64, error) { return h.free, nil }

// TreeFootprintKB sums RSSKB over the fake process table (the fake's
// per-proc RSSKB stands in for its share of phys_footprint).
func (h *fakeHost) TreeFootprintKB(ctx context.Context, pids []int) (int64, error) {
	h.mu.Lock()
	fpErr := h.fpErr
	h.mu.Unlock()
	if fpErr != nil {
		return 0, fpErr
	}
	procs, err := h.Processes(ctx)
	if err != nil {
		return 0, err
	}
	return sumRSSKB(procs, pids), nil
}

func (h *fakeHost) isStopped(pid int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopped[pid]
}

func (h *fakeHost) sigCount(pid int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.signals[pid])
}

func (h *fakeHost) gotSignal(pid int, sig syscall.Signal) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.signals[pid] {
		if s == sig {
			return true
		}
	}
	return false
}

func testPool(t *testing.T, h *fakeHost, reg registry.Registry, class CapacityClass) *Pool {
	t.Helper()
	return NewPool(h, reg, Config{
		Class:      class,
		DevicesDir: t.TempDir(),
		Erase:      func(context.Context, string) error { return nil },
		Logger:     slog.Default(),
	})
}

const udidA = "AAAA1111-2222-3333-4444-555566667777"
const udidB = "BBBB1111-2222-3333-4444-555566667777"

func TestParkThawCachedPIDs(t *testing.T) {
	h := newFakeHost()
	h.setSimTree(udidA, 100, 101, 102)
	p := NewParker(h)

	if err := p.Park(context.Background(), udidA); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{100, 101, 102} {
		if !h.isStopped(pid) {
			t.Errorf("pid %d not stopped", pid)
		}
	}
	if !p.IsParked(udidA) {
		t.Fatal("expected parked")
	}

	// Thaw must use the cached PID list even if the process table changed.
	h.removePID(102)
	if err := p.Thaw(udidA); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{100, 101} {
		if h.isStopped(pid) {
			t.Errorf("pid %d still stopped", pid)
		}
	}
	if p.IsParked(udidA) {
		t.Fatal("expected thawed")
	}
	// Double thaw is a no-op.
	n := h.sigCount(100)
	if err := p.Thaw(udidA); err != nil {
		t.Fatal(err)
	}
	if h.sigCount(100) != n {
		t.Error("double thaw re-signaled")
	}
}

func TestParkNotRunning(t *testing.T) {
	p := NewParker(newFakeHost())
	if err := p.Park(context.Background(), udidA); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("want ErrNotRunning, got %v", err)
	}
}

func TestParkRollsBackOnSignalFailure(t *testing.T) {
	h := newFakeHost()
	h.setSimTree(udidA, 100, 101)
	h.sigErr[101] = errors.New("eperm")
	p := NewParker(h)
	if err := p.Park(context.Background(), udidA); err == nil {
		t.Fatal("want error")
	}
	if !h.gotSignal(100, syscall.SIGCONT) {
		t.Error("rollback did not SIGCONT pid 100")
	}
	if p.IsParked(udidA) {
		t.Error("half-failed park recorded as parked")
	}
}

func TestLoadGate(t *testing.T) {
	h := newFakeHost()
	h.load = 20 // > 2 * 8 cores
	reg := registry.NewMock()
	pool := testPool(t, h, reg, IntelClass)
	if err := pool.GateBoot(context.Background(), ""); !errors.Is(err, ErrLoadTooHigh) {
		t.Fatalf("want ErrLoadTooHigh, got %v", err)
	}
}

func TestFreeSpaceGate(t *testing.T) {
	h := newFakeHost()
	h.free = 1 << 30 // 1 GiB < 5 GiB
	pool := testPool(t, h, registry.NewMock(), IntelClass)
	if err := pool.GateBoot(context.Background(), ""); !errors.Is(err, ErrDiskTooLow) {
		t.Fatalf("want ErrDiskTooLow, got %v", err)
	}
}

func TestRunningCapExcludesParked(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	pool := testPool(t, h, reg, CapacityClass{MaxBootedRunning: 1, MaxParked: 4, MaxConcurrentBoots: 1})

	// Boot one mock target: cap of 1 reached.
	if err := reg.Boot(context.Background(), targets[0].UDID); err != nil {
		t.Fatal(err)
	}
	waitBootedMock(t, reg, targets[0].UDID)
	if err := pool.GateBoot(context.Background(), ""); !errors.Is(err, ErrTooManyRunning) {
		t.Fatalf("want ErrTooManyRunning, got %v", err)
	}

	// Park it (via the parker's book-keeping) and the cap frees up.
	h.setSimTree(targets[0].UDID, 200, 201)
	if err := pool.Parker().Park(context.Background(), targets[0].UDID); err != nil {
		t.Fatal(err)
	}
	if err := pool.GateBoot(context.Background(), ""); err != nil {
		t.Fatalf("parked sim still counted as running: %v", err)
	}
}

func waitBootedMock(t *testing.T, reg registry.Registry, udid string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		st, err := reg.Health(context.Background(), udid)
		if err == nil && st == proto.StateBooted {
			return
		}
	}
	t.Fatalf("mock target %s never booted", udid)
}

func TestGuardShutdownThawsFirst(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	pool := testPool(t, h, reg, IntelClass)
	g := Guard(reg, pool)

	if err := reg.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	waitBootedMock(t, reg, udid)
	h.setSimTree(udid, 300, 301)
	if err := pool.Parker().Park(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	if err := g.Shutdown(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	if h.isStopped(300) || h.isStopped(301) {
		t.Error("shutdown did not thaw first")
	}
	if pool.Parker().IsParked(udid) {
		t.Error("still recorded parked after shutdown")
	}
}

func TestGuardBootThawsParked(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	pool := testPool(t, h, reg, IntelClass)
	g := Guard(reg, pool)

	h.setSimTree(udid, 400, 401)
	if err := pool.Parker().Park(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	if err := g.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	if h.isStopped(400) {
		t.Error("guard boot did not thaw the parked sim")
	}
	if st, _ := g.Health(context.Background(), udid); st != proto.StateBooted && pool.Parker().IsParked(udid) {
		t.Error("parked sim not reported Booted")
	}
}

func TestReapOrphansOnlyOwned(t *testing.T) {
	h := newFakeHost()
	h.setSimTree(udidA, 500, 501)
	h.setSimTree(udidB, 600, 601) // another agent's tree: untouched
	pool := testPool(t, h, registry.NewMock(), IntelClass)

	if err := pool.Parker().Park(context.Background(), udidA); err != nil {
		t.Fatal(err)
	}
	if err := pool.Parker().Thaw(udidA); err != nil {
		t.Fatal(err)
	}
	// Simulate shutdown leaving 501 behind; 500 exits.
	h.removePID(500)
	if err := pool.ReapOrphans(context.Background(), udidA); err != nil {
		t.Fatal(err)
	}
	if !h.gotSignal(501, syscall.SIGKILL) {
		t.Error("orphan 501 not killed")
	}
	if h.gotSignal(600, syscall.SIGKILL) || h.gotSignal(601, syscall.SIGKILL) {
		t.Error("reaper touched a tree it does not own")
	}
	// Never reaped a UDID we never parked.
	if err := pool.ReapOrphans(context.Background(), udidB); err != nil {
		t.Fatal(err)
	}
	if h.gotSignal(600, syscall.SIGKILL) {
		t.Error("reaper killed unowned tree")
	}
}

func TestWatchdogRecyclesRunaway(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	erased := false
	pool := NewPool(h, reg, Config{
		Class:      AppleSiliconClass,
		DevicesDir: t.TempDir(),
		Erase: func(context.Context, string) error {
			erased = true
			return nil
		},
		Logger: slog.Default(),
	})

	if err := reg.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	waitBootedMock(t, reg, udid)
	h.setSimTree(udid, 700, 701)
	pool.mu.Lock()
	pool.members[udid] = true
	pool.mu.Unlock()
	if err := pool.Parker().Park(context.Background(), udid); err != nil {
		t.Fatal(err)
	}

	// Under the cap: nothing happens.
	pool.sweepFootprints(context.Background(), DefaultFootprintCapKB, nil)
	if erased {
		t.Fatal("recycled a healthy sim")
	}

	// Balloon the tree past 3 GiB.
	h.mu.Lock()
	for i := range h.procs {
		if h.procs[i].PID == 701 {
			h.procs[i].RSSKB = 4 << 20
		}
	}
	h.mu.Unlock()
	pool.sweepFootprints(context.Background(), DefaultFootprintCapKB, nil)
	if !erased {
		t.Fatal("runaway sim not recycled")
	}
	if !pool.Parker().IsParked(udid) {
		t.Error("recycled sim not re-parked")
	}
}

func TestWatchdogSkipsReparkOfNonBootedMember(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	pool := NewPool(h, reg, Config{
		Class:      AppleSiliconClass,
		DevicesDir: t.TempDir(),
		Erase:      func(context.Context, string) error { return nil },
		Logger:     slog.Default(),
	})
	pool.mu.Lock()
	pool.members[udid] = true
	pool.mu.Unlock()
	// A shut-down member with a stray launchd_sim tree (external
	// shutdown left orphans): the registry reports Shutdown, but the
	// process table still shows the tree.
	h.setSimTree(udid, 900, 901)

	pool.sweepFootprints(context.Background(), DefaultFootprintCapKB, nil)
	if pool.Parker().IsParked(udid) {
		t.Fatal("watchdog parked a Shutdown-state member's stray tree")
	}
	if h.isStopped(900) || h.isStopped(901) {
		t.Fatal("stray tree was SIGSTOPped")
	}

	// An intentionally-down member is skipped even if the registry says
	// Booted (its tree may still be winding down).
	if err := reg.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	waitBootedMock(t, reg, udid)
	pool.mu.Lock()
	pool.down[udid] = true
	pool.mu.Unlock()
	pool.sweepFootprints(context.Background(), DefaultFootprintCapKB, nil)
	if pool.Parker().IsParked(udid) {
		t.Fatal("watchdog parked an intentionally-down member")
	}

	// A genuinely idle Booted member still gets re-parked.
	pool.mu.Lock()
	delete(pool.down, udid)
	pool.mu.Unlock()
	pool.sweepFootprints(context.Background(), DefaultFootprintCapKB, nil)
	if !pool.Parker().IsParked(udid) {
		t.Fatal("idle Booted member not re-parked")
	}
}

func TestWatchdogFailsOpenOnFootprintError(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	erased := false
	pool := NewPool(h, reg, Config{
		Class:      AppleSiliconClass,
		DevicesDir: t.TempDir(),
		Erase: func(context.Context, string) error {
			erased = true
			return nil
		},
		Logger: slog.Default(),
	})
	if err := reg.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	waitBootedMock(t, reg, udid)
	h.setSimTree(udid, 750, 751)
	h.mu.Lock()
	h.procs[0].RSSKB = 8 << 20 // would be over cap if measured
	h.fpErr = errors.New("footprint: boom")
	h.mu.Unlock()
	pool.mu.Lock()
	pool.members[udid] = true
	pool.mu.Unlock()
	if err := pool.Parker().Park(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	pool.sweepFootprints(context.Background(), DefaultFootprintCapKB, nil)
	if erased {
		t.Fatal("recycled a sim whose footprint could not be measured")
	}
	if !pool.Parker().IsParked(udid) {
		t.Error("unmeasurable sim must stay parked")
	}
}

func TestParseFootprint(t *testing.T) {
	out := []byte(`task_read_for_pid (?/?)  (ffffffff)
footprint: Unable to analyze process with pid 418 (try as root?)
======================================================================
Finder [640]: 64-bit    Footprint: 214434824 B (16384 bytes per page)
======================================================================

Auxiliary data:
    phys_footprint: 214483976 B
    phys_footprint_peak: 250954712 B
======================================================================
launchd_sim [900]: 64-bit    Footprint: 10000000 B (16384 bytes per page)
======================================================================

Auxiliary data:
    phys_footprint: 10485760 B
    phys_footprint_peak: 20971520 B
`)
	kb, n := parseFootprint(out)
	if n != 2 {
		t.Fatalf("parsed %d processes, want 2", n)
	}
	want := int64(214483976/1024 + 10485760/1024)
	if kb != want {
		t.Fatalf("kb = %d, want %d", kb, want)
	}
	if kb, n := parseFootprint([]byte("garbage")); n != 0 || kb != 0 {
		t.Fatalf("garbage parsed: kb=%d n=%d", kb, n)
	}
}

func TestWatchdogSkipsLeased(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	erased := false
	pool := NewPool(h, reg, Config{
		Class:      AppleSiliconClass,
		DevicesDir: t.TempDir(),
		Erase: func(context.Context, string) error {
			erased = true
			return nil
		},
		Logger: slog.Default(),
	})
	h.setSimTree(udid, 800)
	h.mu.Lock()
	h.procs[0].RSSKB = 4 << 20
	h.mu.Unlock()
	pool.mu.Lock()
	pool.members[udid] = true
	pool.mu.Unlock()

	pool.sweepFootprints(context.Background(), DefaultFootprintCapKB, func(string) bool { return true })
	if erased {
		t.Fatal("watchdog recycled a leased sim")
	}
}

func TestSafeShutdownKeepsMembershipAndStaysDown(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	erased := false
	pool := NewPool(h, reg, Config{
		Class:      AppleSiliconClass,
		DevicesDir: t.TempDir(),
		Erase: func(context.Context, string) error {
			erased = true
			return nil
		},
		Logger: slog.Default(),
	})
	if err := reg.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	waitBootedMock(t, reg, udid)
	h.setSimTree(udid, 850, 851)
	pool.mu.Lock()
	pool.members[udid] = true
	pool.mu.Unlock()
	if err := pool.Parker().Park(context.Background(), udid); err != nil {
		t.Fatal(err)
	}

	if err := pool.SafeShutdown(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	if !pool.IsMember(udid) {
		t.Fatal("SafeShutdown dropped pool membership")
	}
	h.removePID(850)
	h.removePID(851)

	// The intentional shutdown sticks: the watchdog must not rebuild.
	pool.sweepFootprints(context.Background(), DefaultFootprintCapKB, nil)
	if erased {
		t.Fatal("watchdog rebuilt an intentionally shut-down member")
	}

	// An explicit boot puts the member back under the watchdog's care.
	g := Guard(reg, pool)
	if err := g.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	if pool.isDown(udid) {
		t.Fatal("explicit boot did not clear the down marker")
	}
}

func TestSafeShutdownDownClearedByRePark(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	pool := testPool(t, h, reg, AppleSiliconClass)
	if err := reg.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	waitBootedMock(t, reg, udid)
	h.setSimTree(udid, 860, 861)
	pool.mu.Lock()
	pool.members[udid] = true
	pool.mu.Unlock()

	if err := pool.SafeShutdown(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	if !pool.isDown(udid) {
		t.Fatal("SafeShutdown did not mark the member down")
	}
	// The lease-end reset re-parks through OnReleased: the sim rejoins.
	if err := pool.OnReleased(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	if pool.isDown(udid) {
		t.Fatal("rePark did not clear the down marker")
	}
	if !pool.Parker().IsParked(udid) {
		t.Fatal("released member not re-parked")
	}
}

// GateBootCheap refuses on a fresh cached running count at the cap but
// never lists targets, and stops trusting the cache once stale.
func TestGateBootCheapUsesCachedRunningCount(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	pool := testPool(t, h, reg, CapacityClass{MaxBootedRunning: 2, MaxConcurrentBoots: 1, MaxParked: 1})
	pool.cfg.MinFreeDisk = 0
	pool.cfg.LoadFactor = 0

	pool.mu.Lock()
	pool.running, pool.runningAt = 2, time.Now()
	pool.mu.Unlock()
	if err := pool.GateBootCheap(udidA); !errors.Is(err, ErrTooManyRunning) {
		t.Fatalf("fresh cache at cap: err = %v, want ErrTooManyRunning", err)
	}

	pool.mu.Lock()
	pool.runningAt = time.Now().Add(-runningCacheTTL - time.Second)
	pool.mu.Unlock()
	if err := pool.GateBootCheap(udidA); err != nil {
		t.Fatalf("stale cache must fail open to the full gate: %v", err)
	}
}

// listCountingRegistry counts List calls, for asserting that the cheap
// gate never triggers a target listing.
type listCountingRegistry struct {
	*registry.MockRegistry
	mu    sync.Mutex
	lists int
}

func (r *listCountingRegistry) List(ctx context.Context) ([]proto.Target, error) {
	r.mu.Lock()
	r.lists++
	r.mu.Unlock()
	return r.MockRegistry.List(ctx)
}

func (r *listCountingRegistry) listCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lists
}

// A cap-refused GateBoot (the boot path, which excludes the boot target
// from its count) must refresh the running-count cache so GateBootCheap
// keeps refusing without listing targets: otherwise the cache (written
// only via /v0/status probes) goes stale after one TTL and every wait
// poll falls back to the full listing gate.
func TestGateBootRefusalRefreshesCheapGateCache(t *testing.T) {
	h := newFakeHost()
	reg := &listCountingRegistry{MockRegistry: registry.NewMock()}
	targets, _ := reg.List(context.Background())
	booted := targets[0].UDID
	pool := testPool(t, h, reg, CapacityClass{MaxBootedRunning: 1, MaxConcurrentBoots: 1, MaxParked: 1})
	pool.cfg.MinFreeDisk = 0
	pool.cfg.LoadFactor = 0
	if err := reg.Boot(context.Background(), booted); err != nil {
		t.Fatal(err)
	}
	waitBootedMock(t, reg.MockRegistry, booted)

	if err := pool.GateBoot(context.Background(), udidA); !errors.Is(err, ErrTooManyRunning) {
		t.Fatalf("full gate at cap: err = %v, want ErrTooManyRunning", err)
	}
	before := reg.listCount()
	for i := 0; i < 5; i++ {
		if err := pool.GateBootCheap(udidA); !errors.Is(err, ErrTooManyRunning) {
			t.Fatalf("cheap gate after full refusal: err = %v, want ErrTooManyRunning", err)
		}
	}
	if n := reg.listCount(); n != before {
		t.Fatalf("cheap gate listed targets: %d lists, want %d", n, before)
	}
}

// A pool boot path (rePark via OnReleased) must report the boot so the
// server can drop the stale shutdown-ledger entry the boot undid —
// these boots never pass through the server's own boot handler.
func TestRePark_ReportsBoot(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	pool := testPool(t, h, reg, AppleSiliconClass)
	var mu sync.Mutex
	var booted []string
	pool.SetBootReporter(func(u string) {
		mu.Lock()
		booted = append(booted, u)
		mu.Unlock()
	})
	if err := reg.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	waitBootedMock(t, reg, udid)
	h.setSimTree(udid, 870, 871)
	pool.mu.Lock()
	pool.members[udid] = true
	pool.mu.Unlock()

	if err := pool.SafeShutdown(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	if err := pool.OnReleased(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, u := range booted {
		if u == udid {
			return
		}
	}
	t.Fatalf("boot reporter never saw %s (got %v)", udid, booted)
}

// bootFailReg fails Boot while fail is set, for exercising the
// background-boot failure path.
type bootFailReg struct {
	*registry.MockRegistry
	mu   sync.Mutex
	fail bool
}

func (r *bootFailReg) setFail(v bool) {
	r.mu.Lock()
	r.fail = v
	r.mu.Unlock()
}

func (r *bootFailReg) Boot(ctx context.Context, udid string) error {
	r.mu.Lock()
	fail := r.fail
	r.mu.Unlock()
	if fail {
		return errors.New("simctl boot: exploded")
	}
	return r.MockRegistry.Boot(ctx, udid)
}

func TestBootAsyncSurfacesBackgroundFailure(t *testing.T) {
	h := newFakeHost()
	reg := &bootFailReg{MockRegistry: registry.NewMock(), fail: true}
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	pool := testPool(t, h, reg, AppleSiliconClass)

	// The request is accepted (Boot is contractually async)...
	if err := pool.BootAsync(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	// ...and the background failure is recorded.
	recorded := func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return pool.bootErr[udid] != nil
	}
	for i := 0; i < 200 && !recorded(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !recorded() {
		t.Fatal("background boot failure never recorded")
	}

	// The next boot attempt surfaces (and clears) it.
	err := pool.BootAsync(context.Background(), udid)
	if err == nil || !strings.Contains(err.Error(), "exploded") {
		t.Fatalf("background failure not surfaced: %v", err)
	}

	// A retry after that proceeds normally.
	reg.setFail(false)
	if err := pool.BootAsync(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	// The boot itself is backgrounded: give it real time to land.
	booted := func() bool {
		st, err := reg.Health(context.Background(), udid)
		return err == nil && st == proto.StateBooted
	}
	for i := 0; i < 500 && !booted(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !booted() {
		t.Fatal("retried boot never reached Booted")
	}
	for i := 0; i < 200 && recorded(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if recorded() {
		t.Fatal("successful boot left a recorded failure")
	}

	// A stale failure behind an already-Booted sim (e.g. brought up by
	// a watchdog rebuild) is dropped, not surfaced.
	pool.setBootErr(udid, errors.New("stale"))
	if err := pool.BootAsync(context.Background(), udid); err != nil {
		t.Fatalf("stale failure surfaced for a Booted sim: %v", err)
	}
	if recorded() {
		t.Fatal("already-Booted fast path did not clear the stale failure")
	}
}

func TestAddThawsFrozenParkedSim(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	slimSawStopped := false
	pool := NewPool(h, reg, Config{
		Class:      AppleSiliconClass,
		DevicesDir: t.TempDir(),
		Erase:      func(context.Context, string) error { return nil },
		Slim: func(_ context.Context, u string) error {
			// Production simslim spawns into the sim: against a
			// SIGSTOPped tree it would hang.
			if h.isStopped(1000) {
				slimSawStopped = true
			}
			return nil
		},
		Logger: slog.Default(),
	})
	if err := reg.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	waitBootedMock(t, reg, udid)
	h.setSimTree(udid, 1000, 1001)
	// A previous Add attempt parked the tree but failed on the way out:
	// the entry survives in the Parker and the sim reports Booted.
	if err := pool.Parker().Park(context.Background(), udid); err != nil {
		t.Fatal(err)
	}

	if err := pool.Add(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	if slimSawStopped {
		t.Fatal("Add ran slim against a frozen tree")
	}
	if !pool.Parker().IsParked(udid) {
		t.Fatal("added sim not parked")
	}
}

func TestWatchdogRebuildGatedAndBackedOff(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	erased := false
	pool := NewPool(h, reg, Config{
		Class:      AppleSiliconClass,
		DevicesDir: t.TempDir(),
		Erase: func(_ context.Context, u string) error {
			erased = true
			// Stand in for the rebuild's boot bringing the tree up.
			h.setSimTree(u, 990, 991)
			return nil
		},
		Logger: slog.Default(),
	})
	pool.mu.Lock()
	pool.members[udid] = true
	pool.mu.Unlock()

	// A shut-down member (no tree) on an overloaded host: the gates
	// must refuse BEFORE the destructive erase, and set a backoff.
	h.load = 100
	pool.sweepFootprints(context.Background(), DefaultFootprintCapKB, nil)
	if erased {
		t.Fatal("gate-refused rebuild still erased the sim")
	}
	if pool.rebuildDue(udid) {
		t.Fatal("gate refusal did not set a rebuild backoff")
	}

	// Load recovers, but the backoff hasn't elapsed: still no rebuild.
	h.load = 1
	pool.sweepFootprints(context.Background(), DefaultFootprintCapKB, nil)
	if erased {
		t.Fatal("rebuild retried before the backoff elapsed")
	}

	// Backoff elapsed: the rebuild runs and clears the backoff state.
	pool.mu.Lock()
	pool.rebuildNext[udid] = time.Now().Add(-time.Second)
	pool.mu.Unlock()
	pool.sweepFootprints(context.Background(), DefaultFootprintCapKB, nil)
	if !erased {
		t.Fatal("due rebuild did not run")
	}
	pool.mu.Lock()
	_, backedOff := pool.rebuildNext[udid]
	pool.mu.Unlock()
	if backedOff {
		t.Fatal("successful rebuild did not clear the backoff")
	}
}

func TestRecoverFrozenParks(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	h.setSimTree(udid, 870, 871)
	h.setSimTree("OTHER-UDID-0000", 880, 881)

	// A previous daemon parks both trees, then dies uncleanly: its
	// in-memory ledger is gone but the trees stay SIGSTOPped.
	old := NewParker(h)
	if err := old.Park(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	if err := old.Park(context.Background(), "OTHER-UDID-0000"); err != nil {
		t.Fatal(err)
	}

	// The next daemon recovers only its configured pool sims.
	pool := testPool(t, h, reg, AppleSiliconClass)
	pool.RecoverFrozenParks(context.Background(), []string{" " + udid + " ", ""})
	if h.isStopped(870) || h.isStopped(871) {
		t.Fatal("configured pool tree not thawed")
	}
	if !h.isStopped(880) || !h.isStopped(881) {
		t.Fatal("recovery touched a tree outside the configured pool")
	}

	// Idempotent when nothing is frozen.
	pool.RecoverFrozenParks(context.Background(), []string{udid})
	if h.isStopped(870) {
		t.Fatal("second recovery pass re-froze the tree")
	}
}

func TestThawFailureKeepsParkedEntry(t *testing.T) {
	h := newFakeHost()
	h.setSimTree(udidA, 900, 901)
	p := NewParker(h)
	if err := p.Park(context.Background(), udidA); err != nil {
		t.Fatal(err)
	}
	h.sigErr[901] = errors.New("eperm")
	if err := p.Thaw(udidA); err == nil {
		t.Fatal("want error")
	}
	if !p.IsParked(udidA) {
		t.Fatal("failed thaw dropped the parked entry")
	}
	// Once the signal succeeds, thaw completes and clears the entry.
	delete(h.sigErr, 901)
	if err := p.Thaw(udidA); err != nil {
		t.Fatal(err)
	}
	if p.IsParked(udidA) {
		t.Fatal("expected thawed")
	}
}

func TestWatchdogSkipsBusy(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	pool := testPool(t, h, reg, AppleSiliconClass)
	if err := reg.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	waitBootedMock(t, reg, udid)
	h.setSimTree(udid, 950)
	pool.mu.Lock()
	pool.members[udid] = true
	pool.mu.Unlock()

	done := pool.BeginTransition(udid)
	pool.sweepFootprints(context.Background(), DefaultFootprintCapKB, nil)
	if h.isStopped(950) {
		t.Fatal("watchdog parked a sim mid-transition")
	}
	done()
	pool.sweepFootprints(context.Background(), DefaultFootprintCapKB, nil)
	if !h.isStopped(950) {
		t.Fatal("idle member not re-parked after transition ended")
	}
}

func TestParkIdleThawsBackOnMidParkGrant(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	pool := testPool(t, h, reg, AppleSiliconClass)
	h.setSimTree(udid, 970, 971)

	// Normal park works while idle.
	ok, err := pool.parkIdle(context.Background(), udid)
	if err != nil || !ok {
		t.Fatalf("parkIdle: ok=%v err=%v", ok, err)
	}
	if err := pool.Parker().Thaw(udid); err != nil {
		t.Fatal(err)
	}

	// A grant landing between the up-front check and the post-park
	// re-check (grantSeq bumped by OnGrant mid-park) must thaw back.
	seq := pool.grantSeqOf(udid)
	pool.SetLeasedFunc(func(string) bool {
		// Simulate the race: the lease fires during the park itself.
		return pool.grantSeqOf(udid) != seq
	})
	if err := pool.Parker().Park(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	if err := pool.Parker().Thaw(udid); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { pool.OnGrant(udid); close(done) }()
	<-done
	ok, err = pool.parkIdle(context.Background(), udid)
	if err != nil {
		t.Fatal(err)
	}
	if ok || pool.Parker().IsParked(udid) {
		t.Fatal("parkIdle left a leased sim parked")
	}
	if h.isStopped(970) || h.isStopped(971) {
		t.Fatal("leased sim left SIGSTOPped")
	}
}

func TestWatchdogDropsStaleParkedEntry(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	pool := testPool(t, h, reg, AppleSiliconClass)
	h.setSimTree(udid, 980, 981)
	if err := pool.Parker().Park(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	pool.members[udid] = true
	pool.mu.Unlock()

	// The tree dies outside the daemon (crash / external shutdown).
	h.removePID(980)
	h.removePID(981)
	pool.sweepFootprints(context.Background(), DefaultFootprintCapKB, nil)
	if pool.Parker().IsParked(udid) {
		t.Fatal("stale parked entry not dropped")
	}
}

func TestClosedPoolNeverParks(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	pool := testPool(t, h, reg, AppleSiliconClass)
	h.setSimTree(udid, 990, 991)

	pool.Close()
	ok, err := pool.parkIdle(context.Background(), udid)
	if err != nil {
		t.Fatal(err)
	}
	if ok || pool.Parker().IsParked(udid) || h.isStopped(990) {
		t.Fatal("closed pool parked a sim")
	}
}

func TestGuardBootRefusedDoesNotMarkAwake(t *testing.T) {
	h := newFakeHost()
	h.free = 1 << 20 // 1 MiB free: the disk gate refuses every boot
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	pool := testPool(t, h, reg, AppleSiliconClass)
	pool.mu.Lock()
	pool.members[udid] = true
	pool.mu.Unlock()
	guard := Guard(reg, pool)

	if err := guard.Boot(context.Background(), udid); !errors.Is(err, ErrDiskTooLow) {
		t.Fatalf("Boot = %v, want ErrDiskTooLow", err)
	}
	if pool.isAwake(udid) {
		t.Fatal("gate-refused boot armed the wake grace")
	}

	// An accepted boot still earns the grace.
	h.free = 100 << 30
	if err := guard.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	if !pool.isAwake(udid) {
		t.Fatal("accepted boot did not arm the wake grace")
	}
}

func TestGuardBootRefusedDoesNotClearDown(t *testing.T) {
	h := newFakeHost()
	h.free = 1 << 20 // 1 MiB free: the disk gate refuses every boot
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	pool := testPool(t, h, reg, AppleSiliconClass)
	pool.mu.Lock()
	pool.members[udid] = true
	pool.down[udid] = true
	pool.mu.Unlock()
	guard := Guard(reg, pool)

	if err := guard.Boot(context.Background(), udid); !errors.Is(err, ErrDiskTooLow) {
		t.Fatalf("Boot = %v, want ErrDiskTooLow", err)
	}
	if !pool.isDown(udid) {
		t.Fatal("gate-refused boot cancelled the intentional shutdown")
	}

	// An accepted boot hands the member back to the watchdog.
	h.free = 100 << 30
	if err := guard.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	if pool.isDown(udid) {
		t.Fatal("accepted boot left the member marked down")
	}
}

func TestGateBootDisabledGates(t *testing.T) {
	h := newFakeHost()
	h.load = 100.0   // way over 2x 8 cores
	h.free = 1 << 20 // 1 MiB free, way under 5 GiB
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	for _, tt := range targets {
		if err := reg.Boot(context.Background(), tt.UDID); err != nil {
			t.Fatal(err)
		}
	}
	pool := NewPool(h, reg, Config{
		Class:       CapacityClass{MaxBootedRunning: -1, MaxParked: 4, MaxConcurrentBoots: 1},
		LoadFactor:  -1,
		MinFreeDisk: -1,
		DevicesDir:  t.TempDir(),
		Erase:       func(context.Context, string) error { return nil },
		Logger:      slog.Default(),
	})
	if err := pool.GateBoot(context.Background(), ""); err != nil {
		t.Fatalf("disabled gates still refused: %v", err)
	}

	// Sanity: the same host state refuses with all gates at defaults.
	def := testPool(t, h, reg, AppleSiliconClass)
	if err := def.GateBoot(context.Background(), ""); err == nil {
		t.Fatal("default gates did not refuse an overloaded host")
	}
}

func TestAddRetryOfParkedSimNotRefusedByCap(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	pool := testPool(t, h, reg, CapacityClass{MaxBootedRunning: 2, MaxParked: 1, MaxConcurrentBoots: 1})
	if err := reg.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	waitBootedMock(t, reg, udid)
	h.setSimTree(udid, 1100, 1101)
	// A previous Add parked the tree but failed on the way out: the
	// Parker entry fills the (size-1) cap.
	if err := pool.Parker().Park(context.Background(), udid); err != nil {
		t.Fatal(err)
	}

	// The retry must not be refused against its own cap slot.
	if err := pool.Add(context.Background(), udid); err != nil {
		t.Fatalf("retry of a parked sim refused: %v", err)
	}
	if !pool.Parker().IsParked(udid) {
		t.Fatal("retried sim not parked")
	}
}

func TestNewPoolDefaultsMaxParked(t *testing.T) {
	pool := NewPool(newFakeHost(), registry.NewMock(), Config{
		Class:  CapacityClass{MaxBootedRunning: 4, MaxConcurrentBoots: 1},
		Logger: slog.Default(),
	})
	if got, want := pool.Class().MaxParked, DetectClass().MaxParked; got != want {
		t.Fatalf("MaxParked = %d, want detected default %d", got, want)
	}
}

func TestGateBootExcludesBootTarget(t *testing.T) {
	h := newFakeHost()
	reg := registry.NewMock()
	targets, _ := reg.List(context.Background())
	udid := targets[0].UDID
	pool := testPool(t, h, reg, CapacityClass{MaxBootedRunning: 1, MaxParked: 4, MaxConcurrentBoots: 1})
	if err := reg.Boot(context.Background(), udid); err != nil {
		t.Fatal(err)
	}
	waitBootedMock(t, reg, udid)
	// Re-booting the already-Booted sim at the cap stays a no-op.
	if err := pool.GateBoot(context.Background(), udid); err != nil {
		t.Fatalf("gate rejected re-boot of the running target: %v", err)
	}
	// A different sim is still refused.
	if err := pool.GateBoot(context.Background(), ""); !errors.Is(err, ErrTooManyRunning) {
		t.Fatalf("want ErrTooManyRunning, got %v", err)
	}
}

func TestBootAsyncUnknownUDID(t *testing.T) {
	pool := testPool(t, newFakeHost(), registry.NewMock(), AppleSiliconClass)
	if err := pool.BootAsync(context.Background(), "NOPE-0000"); err == nil {
		t.Fatal("BootAsync accepted an unknown UDID")
	}
}

func TestNewPoolDefaultsConcurrentBoots(t *testing.T) {
	pool := testPool(t, newFakeHost(), registry.NewMock(), CapacityClass{MaxBootedRunning: 1, MaxParked: 4})
	if got := pool.Class().MaxConcurrentBoots; got != 1 {
		t.Fatalf("MaxConcurrentBoots not defaulted: %d", got)
	}
}

func TestDetectClassValues(t *testing.T) {
	if IntelClass.MaxBootedRunning != 2 || IntelClass.MaxParked != 6 || IntelClass.MaxConcurrentBoots != 1 {
		t.Error("Intel class drifted from fleet policy")
	}
	if AppleSiliconClass.MaxBootedRunning != 4 || AppleSiliconClass.MaxParked != 4 || AppleSiliconClass.MaxConcurrentBoots != 2 {
		t.Error("Apple Silicon class drifted from fleet policy")
	}
}

func TestParsePS(t *testing.T) {
	out := []byte("  100   1  2048 Ss /usr/libexec/launchd_sim path\n  101 100   512 T  simdaemon --flag\nbadline\n")
	procs, err := parsePS(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 2 || procs[0].PID != 100 || procs[1].PPID != 100 || procs[1].RSSKB != 512 {
		t.Fatalf("bad parse: %+v", procs)
	}
	if procs[0].State != "Ss" || procs[1].State != "T" {
		t.Fatalf("bad states: %+v", procs)
	}
}
