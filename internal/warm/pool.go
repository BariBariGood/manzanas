package warm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

const (
	// bootWait bounds how long a pool boot may take before it is failed.
	bootWait = 5 * time.Minute
	// bootPoll is how often the pool re-checks a booting sim's state.
	bootPoll = 2 * time.Second
	// wakeGrace is how long an explicitly booted/thawed pool member is
	// exempt from the watchdog's idle re-park, so an operator boot isn't
	// silently frozen a minute later. A lease grant or shutdown ends the
	// exemption early.
	wakeGrace = 15 * time.Minute
)

// ErrPoolFull means the parked-pool cap for this host class is reached.
var ErrPoolFull = errors.New("parked pool is at capacity")

// ErrTooManyRunning means the running (booted, un-parked) sim cap is hit.
var ErrTooManyRunning = errors.New("too many running simulators on this host")

// SlimFunc slims a booted simulator (simslim); nil disables slimming.
type SlimFunc func(ctx context.Context, udid string) error

// Config tunes a Pool. Zero-value fields take defaults.
type Config struct {
	Class CapacityClass
	// LoadFactor gates boots when 1-min load > factor * cores.
	// 0 = DefaultLoadFactor; negative disables the load gate.
	LoadFactor float64
	// MinFreeDisk (bytes) refuses boots below this much free space.
	// 0 = DefaultMinFreeDisk; negative disables the disk gate.
	MinFreeDisk int64
	DevicesDir  string // volume checked by the disk gate; default ~/Library/Developer/CoreSimulator/Devices
	Slim        SlimFunc
	// Erase wipes a shut-down simulator; default shells out to
	// `xcrun simctl erase`. Swappable for tests.
	Erase func(ctx context.Context, udid string) error
	// ReapStaleLocks lets the janitor shut down (never delete) unmanaged
	// Booted sims whose fleet lock file under LockDir is stale or
	// missing. Off by default: an operator opts a fleet daemon in.
	ReapStaleLocks bool
	// LockDir holds the fleet lock protocol's per-resource lock files
	// (`sim-<udid>` per simulator); default DefaultLockDir.
	LockDir string
	Logger  *slog.Logger
}

// Pool owns the parked warm sims and the host safety gates. It sits next
// to the lease manager: Guard wraps the registry so every boot passes the
// gates, OnGrant thaws pool sims when leased, and OnReleased re-parks them
// after the post-lease erase.
type Pool struct {
	host Host
	reg  registry.Registry
	prk  *Parker
	cfg  Config
	log  *slog.Logger

	// bootSlots serializes boots per the capacity class.
	bootSlots chan struct{}

	mu      sync.Mutex
	members map[string]bool // sims managed by this pool
	// busy counts in-flight lifecycle transitions (add/reset/re-park/
	// recycle) per UDID so the watchdog never SIGSTOPs or recycles a sim
	// mid-erase/mid-boot/mid-slim.
	busy map[string]int
	// grantSeq counts lease grants per UDID; parkIdle re-checks it after
	// the (slow) park so a grant landing mid-park is thawed right back.
	grantSeq map[string]uint64
	// leased reports live lease state (wired via SetLeasedFunc); nil
	// means "unknown", treated as not leased.
	leased LeasedFunc
	// closed is set by Close so no background path can SIGSTOP a tree
	// after the shutdown-time ThawAll has run.
	closed bool
	// parking counts in-flight Park signal deliveries (seconds each) so
	// Close can join them before its final ThawAll; parkDone (on mu) is
	// signalled when the count drops to zero.
	parking  int
	parkDone *sync.Cond
	// reserve holds a target in the lease manager for the duration of a
	// destructive pool transition (recycle, adoption, re-provision) so a
	// lease can't be granted on a sim mid-wipe. Nil means no lease
	// manager is wired (tests): transitions proceed unreserved.
	reserve   ReserveFunc
	markClean func(udid string)
	// awakeUntil marks members explicitly booted/thawed via the guarded
	// registry: the watchdog leaves them un-parked until the deadline.
	awakeUntil map[string]time.Time
	// down marks members intentionally shut down via the guarded
	// registry (SafeShutdown): the watchdog's shut-down-member rebuild
	// leaves them down instead of booting them right back up. Cleared on
	// an explicit boot and by rePark (lease end / recycle) so the sim
	// rejoins the pool. Membership itself is never dropped: nothing
	// re-adds members after startup, so dropping it would shrink the
	// pool for the daemon's life.
	down map[string]bool
	// rebuildNext/rebuildWait back off the watchdog's shut-down-member
	// rebuild per target: a gate-refused (or otherwise failed) rebuild
	// must not retry a destructive erase every sweep on a host already
	// at its load/disk/running limits.
	rebuildNext map[string]time.Time
	rebuildWait map[string]time.Duration
	// daemonBooted records sims this daemon booted through the gated
	// client boot path (BootAsync) that are NOT pool members, so the
	// janitor can shut them down when they go idle. Sims booted outside
	// the daemon are never in this ledger and never touched.
	daemonBooted map[string]bool
	// idleSince records when the janitor first observed a daemon-booted
	// sim idle, so a reclaim only happens after a full grace period.
	idleSince map[string]time.Time
	// staleSince records when the janitor first observed an unmanaged
	// sim's fleet lock stale/missing, so a stale-lock reap only happens
	// after the observation persists across a full grace period.
	staleSince map[string]time.Time
	// lastLockDecision remembers the last logged reap decision per
	// unmanaged sim so a stuck sim doesn't spam the log every sweep.
	lastLockDecision map[string]string
	// reapedN counts stale-lock reaps for /v0/status.
	reapedN int
	// lockDirWarned suppresses repeated missing-lock-dir warnings while
	// the outage persists.
	lockDirWarned bool
	// configuredPool is the set of UDIDs configured for the pool,
	// including sims not yet adopted by provisioning.
	configuredPool map[string]bool
	// lastUnmanaged is the previous sweep's unmanaged-sim set key, used
	// to log changes only.
	lastUnmanaged string
	// unmanagedN caches the last unmanaged-booted-sim count alongside
	// running/runningAt for /v0/status.
	unmanagedN int
	// bootErr records the last background BootAsync failure per target.
	// Boot is contractually async (callers get 202 and poll), so a boot
	// that dies in the background would otherwise be invisible: clients
	// poll a sim that never reaches Booted, forever. The next boot
	// attempt surfaces (and clears) the recorded failure.
	bootErr map[string]error
	// running/runningAt cache the last runningCount result so Status can
	// answer without shelling out to simctl per probe.
	running   int
	runningAt time.Time
	// streaming reports whether a viewer has an open stream on the
	// target (wired via SetStreamingFunc); nil means "unknown", treated
	// as not streaming. Parking freezes frame capture, so idle re-park
	// skips streamed sims.
	streaming StreamingFunc
	// onShutdown is invoked after each successful pool-driven simulator
	// shutdown (wired via SetShutdownFunc); nil means no listener.
	onShutdown func(udid string)
	// reporter records who shut a target down and why (wired via
	// SetShutdownReporter); nil means no listener. The server surfaces
	// it to leaseholders that later find their target not booted.
	reporter func(udid, actor, reason string)
	// bootReporter is invoked whenever a pool path observes a target
	// reach Booted (wired via SetBootReporter); nil means no listener.
	// The server uses it to drop stale shutdown-ledger entries that the
	// boot has undone (rePark/Recycle boots never pass through the
	// server's own boot handler).
	bootReporter func(udid string)
	// recording reports whether a target has a live (or finalizing)
	// video recording (wired via SetRecordingFunc); nil means "unknown",
	// treated as not recording. Parking or tearing a sim down
	// mid-recording hangs the recordVideo child, so those paths skip
	// recording sims.
	recording func(udid string) bool
}

// NewPool builds a Pool over the raw (un-guarded) registry.
func NewPool(host Host, reg registry.Registry, cfg Config) *Pool {
	if cfg.Class == (CapacityClass{}) {
		cfg.Class = DetectClass()
	}
	if cfg.Class.MaxConcurrentBoots <= 0 {
		cfg.Class.MaxConcurrentBoots = 1
	}
	if cfg.Class.MaxParked <= 0 {
		cfg.Class.MaxParked = DetectClass().MaxParked
	}
	if cfg.LoadFactor == 0 {
		cfg.LoadFactor = DefaultLoadFactor
	}
	if cfg.MinFreeDisk == 0 {
		cfg.MinFreeDisk = int64(DefaultMinFreeDisk)
	}
	if cfg.DevicesDir == "" {
		cfg.DevicesDir = defaultDevicesDir()
	}
	if cfg.Erase == nil {
		cfg.Erase = func(ctx context.Context, udid string) error {
			return runSimctl(ctx, "erase", udid)
		}
	}
	if cfg.LockDir == "" {
		cfg.LockDir = DefaultLockDir
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	p := &Pool{
		host:         host,
		reg:          reg,
		prk:          NewParker(host),
		cfg:          cfg,
		log:          cfg.Logger,
		bootSlots:    make(chan struct{}, cfg.Class.MaxConcurrentBoots),
		members:      make(map[string]bool),
		busy:         make(map[string]int),
		grantSeq:     make(map[string]uint64),
		awakeUntil:   make(map[string]time.Time),
		down:         make(map[string]bool),
		rebuildNext:  make(map[string]time.Time),
		rebuildWait:  make(map[string]time.Duration),
		bootErr:      make(map[string]error),
		daemonBooted: make(map[string]bool),
		idleSince:    make(map[string]time.Time),
		staleSince:   make(map[string]time.Time),

		lastLockDecision: make(map[string]string),
		configuredPool:   make(map[string]bool),
	}
	p.parkDone = sync.NewCond(&p.mu)
	return p
}

// defaultDevicesDir picks the volume the disk gate measures: the
// CoreSimulator devices dir when it exists, else the home dir, else "/"
// — always a path that exists so `df` can't fail and brick every boot.
func defaultDevicesDir() string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		dir := filepath.Join(home, "Library", "Developer", "CoreSimulator", "Devices")
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		if _, err := os.Stat(home); err == nil {
			return home
		}
	}
	return "/"
}

// Parker exposes the underlying parker (Guard and tests use it).
func (p *Pool) Parker() *Parker { return p.prk }

// Class returns the capacity class in force.
func (p *Pool) Class() CapacityClass { return p.cfg.Class }

// LoadFactor returns the effective load gate factor (<= 0 = disabled).
func (p *Pool) LoadFactor() float64 { return p.cfg.LoadFactor }

// MinFreeDisk returns the effective disk gate threshold in bytes
// (<= 0 = disabled).
func (p *Pool) MinFreeDisk() int64 { return p.cfg.MinFreeDisk }

// IsMember reports whether the UDID is managed by the pool.
func (p *Pool) IsMember(udid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.members[udid]
}

// SetConfiguredPool records the UDIDs configured for the warm pool so
// the stale-lock reaper never treats a pool sim awaiting adoption
// (provisioning is asynchronous and can outlast the reap grace) as an
// abandoned unmanaged sim.
func (p *Pool) SetConfiguredPool(udids []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.configuredPool = make(map[string]bool, len(udids))
	for _, u := range udids {
		if u = strings.TrimSpace(u); u == "" {
			continue
		}
		p.configuredPool[u] = true
	}
}

// isConfiguredPool reports whether the UDID is in the configured pool
// set, member or not.
func (p *Pool) isConfiguredPool(udid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.configuredPool[udid]
}

// BeginTransition marks a sim as mid-lifecycle (erase/boot/slim/park in
// flight) so the footprint watchdog leaves it alone; the returned func
// ends the transition. The post-lease reset path wraps its whole
// reset+re-park sequence in one transition. Re-entrant per UDID.
func (p *Pool) BeginTransition(udid string) func() {
	p.mu.Lock()
	p.busy[udid]++
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		if p.busy[udid]--; p.busy[udid] <= 0 {
			delete(p.busy, udid)
		}
		p.mu.Unlock()
	}
}

// ReserveFunc holds a target against lease grants for the duration of a
// destructive pool transition; ok=false means the target is already
// leased/held and the transition must be skipped. takeover=true means
// the hold displaced a failed-reset quarantine: the target may still
// carry the previous holder's data and MUST be erased before it is
// released as rebuilt. release frees it: rebuilt=true makes the target
// grantable again, rebuilt=false keeps it quarantined (the rebuild
// failed, so it must not be handed out dead).
type ReserveFunc func(udid string) (release func(rebuilt bool), takeover bool, ok bool)

// SetReserveFunc wires the lease manager's target reservation into the
// pool. Set before serving traffic.
func (p *Pool) SetReserveFunc(fn ReserveFunc) {
	p.mu.Lock()
	p.reserve = fn
	p.mu.Unlock()
}

// reserveTarget acquires the lease-manager hold, if wired.
func (p *Pool) reserveTarget(udid string) (release func(rebuilt bool), takeover bool, ok bool) {
	p.mu.Lock()
	fn := p.reserve
	p.mu.Unlock()
	if fn == nil {
		return func(bool) {}, false, true
	}
	return fn(udid)
}

// SetMarkCleanFunc wires the lease manager's dirty-state clearing into
// the pool: rebuild paths that erase a target under a plain (non-takeover)
// hold call it so the freshly wiped sim is not re-erased pre-grant. Set
// before serving traffic.
func (p *Pool) SetMarkCleanFunc(fn func(udid string)) {
	p.mu.Lock()
	p.markClean = fn
	p.mu.Unlock()
}

// notifyClean reports a successfully erased target to the lease manager.
func (p *Pool) notifyClean(udid string) {
	p.mu.Lock()
	fn := p.markClean
	p.mu.Unlock()
	if fn != nil {
		fn(udid)
	}
}

// SetLeasedFunc wires live lease state into the pool so no park path
// ever SIGSTOPs a sim its holder is using. Set before serving traffic.
func (p *Pool) SetLeasedFunc(fn LeasedFunc) {
	p.mu.Lock()
	p.leased = fn
	p.mu.Unlock()
}

func (p *Pool) isLeased(udid string) bool {
	p.mu.Lock()
	fn := p.leased
	p.mu.Unlock()
	return fn != nil && fn(udid)
}

// StreamingFunc reports whether a target has an open media stream.
type StreamingFunc func(udid string) bool

// SetStreamingFunc wires live stream state into the pool so an idle
// re-park never SIGSTOPs a tree a viewer is capturing frames from.
// Set before serving traffic.
func (p *Pool) SetStreamingFunc(fn StreamingFunc) {
	p.mu.Lock()
	p.streaming = fn
	p.mu.Unlock()
}

// SetShutdownFunc wires a callback the pool invokes after every
// successful simulator shutdown it performs (recycle, adoption takeover,
// SafeShutdown). Set before serving traffic. Used to clear the
// recorder's per-target poisoned flag: a shutdown+boot cycle is what
// recovers a wedged host recording session.
func (p *Pool) SetShutdownFunc(fn func(udid string)) {
	p.mu.Lock()
	p.onShutdown = fn
	p.mu.Unlock()
}

// SetShutdownReporter wires a callback the pool invokes when a janitor
// or watchdog decision shuts a simulator down, with the deciding actor
// and the reason. Set before serving traffic.
func (p *Pool) SetShutdownReporter(fn func(udid, actor, reason string)) {
	p.mu.Lock()
	p.reporter = fn
	p.mu.Unlock()
}

func (p *Pool) reportShutdown(udid, actor, reason string) {
	p.mu.Lock()
	fn := p.reporter
	p.mu.Unlock()
	if fn != nil {
		fn(udid, actor, reason)
	}
}

// SetBootReporter wires a callback the pool invokes whenever one of its
// synchronous boot paths (Add, rePark, Recycle) observes the target reach
// Booted. Set before serving traffic.
func (p *Pool) SetBootReporter(fn func(udid string)) {
	p.mu.Lock()
	p.bootReporter = fn
	p.mu.Unlock()
}

func (p *Pool) reportBoot(udid string) {
	p.mu.Lock()
	fn := p.bootReporter
	p.mu.Unlock()
	if fn != nil {
		fn(udid)
	}
}

func (p *Pool) notifyShutdown(udid string) {
	p.mu.Lock()
	fn := p.onShutdown
	p.mu.Unlock()
	if fn != nil {
		fn(udid)
	}
}

func (p *Pool) isStreaming(udid string) bool {
	p.mu.Lock()
	fn := p.streaming
	p.mu.Unlock()
	return fn != nil && fn(udid)
}

// SetRecordingFunc wires live recording state into the pool so an idle
// re-park or recycle never freezes or tears down a sim whose video is
// still being written or finalized (a lease-end stop runs asynchronously
// for a few seconds after the lease is gone). Set before serving traffic.
func (p *Pool) SetRecordingFunc(fn func(udid string) bool) {
	p.mu.Lock()
	p.recording = fn
	p.mu.Unlock()
}

func (p *Pool) isRecording(udid string) bool {
	p.mu.Lock()
	fn := p.recording
	p.mu.Unlock()
	return fn != nil && fn(udid)
}

// MarkAwake exempts an explicitly booted/thawed member from the
// watchdog's idle re-park for wakeGrace.
func (p *Pool) MarkAwake(udid string) {
	p.mu.Lock()
	p.awakeUntil[udid] = time.Now().Add(wakeGrace)
	p.mu.Unlock()
}

func (p *Pool) isAwake(udid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Now().Before(p.awakeUntil[udid])
}

func (p *Pool) clearAwake(udid string) {
	p.mu.Lock()
	delete(p.awakeUntil, udid)
	p.mu.Unlock()
}

func (p *Pool) isDown(udid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.down[udid]
}

func (p *Pool) clearDown(udid string) {
	p.mu.Lock()
	delete(p.down, udid)
	p.mu.Unlock()
}

func (p *Pool) grantSeqOf(udid string) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.grantSeq[udid]
}

// parkIdle parks a sim only while it stays un-leased: it skips leased
// sims up front, and if a lease lands during the (slow, ps-walking) park
// it thaws right back so the holder never sees a frozen sim. Reports
// whether the sim ended up parked.
func (p *Pool) parkIdle(ctx context.Context, udid string) (bool, error) {
	// The closed check and in-flight registration are atomic under mu:
	// either Close sees this park and joins it, or this park sees closed
	// and skips — a SIGSTOP can never land after Close's final ThawAll.
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return false, nil
	}
	p.parking++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		if p.parking--; p.parking == 0 {
			p.parkDone.Broadcast()
		}
		p.mu.Unlock()
	}()
	// Snapshot the transition depth first — before the idleness checks —
	// so a NEW transition starting anywhere after this point (a
	// post-release reset beginning shutdown/erase) raises the depth above
	// the snapshot and triggers the thaw-back below; taken later it could
	// already include the very transition it must detect. Callers already
	// inside their own transition (Add/rePark) keep the same depth
	// throughout.
	busy0 := p.busyLevel(udid)
	if p.isLeased(udid) || p.isStreaming(udid) || p.isRecording(udid) {
		return false, nil
	}
	seq := p.grantSeqOf(udid)
	if err := p.prk.Park(ctx, udid); err != nil {
		return false, err
	}
	if p.isClosed() || p.grantSeqOf(udid) != seq || p.isLeased(udid) || p.busyLevel(udid) > busy0 || p.isAwake(udid) || p.isStreaming(udid) || p.isRecording(udid) {
		p.log.Info("lease, transition, stream, recording, or explicit boot landed mid-park (or pool closing); thawing back", "udid", udid)
		if err := p.prk.Thaw(udid); err != nil {
			return true, err
		}
		return false, nil
	}
	return true, nil
}

// isBusy reports whether the sim has a lifecycle transition in flight.
func (p *Pool) isBusy(udid string) bool {
	return p.busyLevel(udid) > 0
}

func (p *Pool) busyLevel(udid string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.busy[udid]
}

// GateBoot runs the safety gates a boot must pass: free disk, host load,
// and the running-sim cap. forUDID is the sim about to be booted: it is
// excluded from the running count so booting an already-Booted sim stays
// the idempotent no-op the registry promises ("" excludes nothing). It
// does NOT reserve a boot slot.
func (p *Pool) GateBoot(ctx context.Context, forUDID string) error {
	if p.cfg.MinFreeDisk > 0 {
		if err := checkDisk(p.host, p.cfg.DevicesDir, uint64(p.cfg.MinFreeDisk)); err != nil {
			if !errors.Is(err, ErrDiskTooLow) {
				// The volume couldn't be measured (path missing, df failed):
				// fail open — only a confirmed-low volume refuses boots.
				p.log.Warn("disk gate: free-space check failed; skipping", "path", p.cfg.DevicesDir, "err", err)
			} else {
				return err
			}
		}
	}
	if p.cfg.LoadFactor > 0 {
		if err := checkLoad(p.host, p.cfg.LoadFactor); err != nil {
			if !errors.Is(err, ErrLoadTooHigh) {
				// The load couldn't be measured: fail open, matching the
				// disk gate — only a confirmed-high load refuses boots.
				p.log.Warn("load gate: load check failed; skipping", "err", err)
			} else {
				return err
			}
		}
	}
	if p.cfg.Class.MaxBootedRunning > 0 {
		running, err := p.runningCount(ctx, forUDID)
		if err != nil {
			return err
		}
		if running >= p.cfg.Class.MaxBootedRunning {
			return fmt.Errorf("%w: %d running >= cap %d", ErrTooManyRunning, running, p.cfg.Class.MaxBootedRunning)
		}
	}
	return nil
}

// GateBootCheap is the boot-wait pre-check: the disk and load gates
// (cheap syscalls) plus the cached running count — never a fresh target
// listing, so an overloaded host isn't asked for a full simctl list per
// waiter per poll. The cache is only trusted as a refusal signal: when
// it is stale or under the cap the caller proceeds to the full gated
// boot, which recounts authoritatively.
func (p *Pool) GateBootCheap(forUDID string) error {
	if p.cfg.MinFreeDisk > 0 {
		if err := checkDisk(p.host, p.cfg.DevicesDir, uint64(p.cfg.MinFreeDisk)); err != nil && errors.Is(err, ErrDiskTooLow) {
			return err
		}
	}
	if p.cfg.LoadFactor > 0 {
		if err := checkLoad(p.host, p.cfg.LoadFactor); err != nil && errors.Is(err, ErrLoadTooHigh) {
			return err
		}
	}
	if p.cfg.Class.MaxBootedRunning > 0 {
		p.mu.Lock()
		n, at := p.running, p.runningAt
		p.mu.Unlock()
		if !at.IsZero() && time.Since(at) <= runningCacheTTL && n >= p.cfg.Class.MaxBootedRunning {
			return fmt.Errorf("%w: %d running >= cap %d (cached)", ErrTooManyRunning, n, p.cfg.Class.MaxBootedRunning)
		}
	}
	return nil
}

// runningCount counts Booted sims that are not parked (parked sims are
// SIGSTOPped and cost no CPU, so they don't consume running capacity).
// exclude names a sim that must not count against the cap (the boot
// target itself).
func (p *Pool) runningCount(ctx context.Context, exclude string) (int, error) {
	targets, err := p.reg.List(ctx)
	if err != nil {
		return 0, err
	}
	n, total, unmanaged := 0, 0, 0
	p.mu.Lock()
	members := make(map[string]bool, len(p.members))
	for u := range p.members {
		members[u] = true
	}
	booted := make(map[string]bool, len(p.daemonBooted))
	for u := range p.daemonBooted {
		booted[u] = true
	}
	p.mu.Unlock()
	for _, t := range targets {
		// Only simulators consume host boot capacity: a connected physical
		// device also reports Booted but costs the host nothing.
		if t.Kind != proto.TargetSimulator {
			continue
		}
		if t.State == proto.StateBooted && !p.prk.IsParked(t.UDID) {
			total++
			if t.UDID != exclude {
				n++
			}
			if !members[t.UDID] && !booted[t.UDID] && t.UDID != exclude {
				unmanaged++
			}
		}
	}
	// Every count refreshes the cache — boot-path calls (non-empty
	// exclude) included, so GateBootCheap keeps a fresh refusal signal
	// between polls. The cached figure is the unadjusted total; it is
	// conservative by at most one (the excluded boot target itself),
	// which is fine for a refusal-only signal.
	p.mu.Lock()
	p.running, p.runningAt = total, time.Now()
	if exclude == "" {
		p.unmanagedN = unmanaged
	}
	p.mu.Unlock()
	return n, nil
}

// BootAsync is the registry-facing boot path (Guard uses it): it
// validates the target and runs the gates synchronously so callers get
// not-found and gate refusals immediately, then boots in the background
// under the boot-slot serialization, re-checking the gates once the
// slot is held (a storm of concurrent requests must not all pass a cap
// checked before queueing). It preserves the registry contract that
// Boot is asynchronous (callers poll for state); a boot that fails
// after acceptance surfaces as the sim never reaching Booted.
func (p *Pool) BootAsync(ctx context.Context, udid string) error {
	t, err := p.reg.Get(ctx, udid)
	if err != nil {
		return err
	}
	// Physical devices cannot be booted: bypass the async pool machinery
	// so the registry's typed refusal reaches the caller synchronously
	// instead of being parked as a background boot error.
	if t.Kind == proto.TargetDevice {
		return p.reg.Boot(ctx, udid)
	}
	// Already Booted: keep the registry's idempotent no-op contract; the
	// load/disk gates only guard boots that add work to the host. Any
	// recorded failure is obsolete — the sim is demonstrably up (it may
	// have been booted by another path, e.g. a watchdog rebuild).
	if t.State == proto.StateBooted {
		p.clearBootErr(udid)
		return nil
	}
	// Surface (and clear) the previous request's background failure:
	// without this a boot that died after its 202 is indistinguishable
	// from one still in flight, and clients poll forever. Clearing lets
	// this attempt proceed on the caller's retry.
	if berr := p.takeBootErr(udid); berr != nil {
		return fmt.Errorf("boot %s: previous boot failed in the background: %w", udid, berr)
	}
	if err := p.GateBoot(ctx, udid); err != nil {
		return err
	}
	// The in-flight boot is a lifecycle transition: the watchdog must not
	// judge (or erase/rebuild) a member whose tree hasn't appeared yet.
	done := p.BeginTransition(udid)
	go func() {
		defer done()
		bctx, cancel := context.WithTimeout(context.Background(), bootWait)
		defer cancel()
		select {
		case p.bootSlots <- struct{}{}:
		case <-bctx.Done():
			p.log.Warn("gated boot never got a slot", "udid", udid)
			p.setBootErr(udid, fmt.Errorf("no boot slot within %s", bootWait))
			return
		}
		defer func() { <-p.bootSlots }()
		// Re-check with the slot held: siblings queued behind us may
		// have filled the running cap or raised the load meanwhile.
		if err := p.GateBoot(bctx, udid); err != nil {
			p.log.Warn("gated boot refused after waiting for a slot", "udid", udid, "err", err)
			p.setBootErr(udid, err)
			return
		}
		if err := p.reg.Boot(bctx, udid); err != nil {
			p.log.Warn("gated boot failed", "udid", udid, "err", err)
			p.setBootErr(udid, err)
			return
		}
		// The boot is issued: record ownership now, not after the sim is
		// observed Booted, so a slow boot that outlives the poll below
		// still has an owner to reclaim it. A sim that never comes up is
		// harmless in the ledger — the reclaim's Health check drops the
		// record.
		p.markDaemonBooted(udid)
		// Hold the slot until Booted so concurrent boots are truly
		// serialized, not just their simctl invocations.
		if err := p.waitBooted(bctx, udid); err != nil {
			p.log.Warn("gated boot did not reach Booted", "udid", udid, "err", err)
			p.setBootErr(udid, err)
			return
		}
		p.clearBootErr(udid)
		p.reportBoot(udid)
	}()
	return nil
}

func (p *Pool) setBootErr(udid string, err error) {
	p.mu.Lock()
	p.bootErr[udid] = err
	p.mu.Unlock()
}

// takeBootErr returns and clears the recorded background boot failure.
func (p *Pool) takeBootErr(udid string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.bootErr[udid]
	delete(p.bootErr, udid)
	return err
}

func (p *Pool) clearBootErr(udid string) {
	p.mu.Lock()
	delete(p.bootErr, udid)
	p.mu.Unlock()
}

// bootGated acquires a boot slot (serializing boots per the class), runs
// the gates, boots, and waits until the sim reports Booted.
func (p *Pool) bootGated(ctx context.Context, udid string) error {
	select {
	case p.bootSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.bootSlots }()
	// Already Booted: no new host work, so the load/disk gates must not
	// block the slim+park that follows (same contract as BootAsync).
	// The lookup error propagates typed (*registry.NotFoundError for an
	// unknown UDID) so callers like the startup provisioner can tell a
	// mistyped sim from a transient failure — simctl boot itself only
	// reports "Invalid device" as a plain error.
	t, err := p.reg.Get(ctx, udid)
	if err != nil {
		return err
	}
	if t.State == proto.StateBooted {
		// A frozen-parked sim reports Booted (SIGSTOP doesn't change the
		// CoreSimulator state): thaw it before callers slim or hand it
		// over — `simctl spawn` against a stopped tree hangs. This
		// covers a retried Add whose previous attempt parked the tree
		// but failed on the way out (Parker keeps the entry).
		if p.prk.IsParked(udid) {
			if err := p.prk.Thaw(udid); err != nil {
				return fmt.Errorf("boot %s: thaw parked: %w", udid, err)
			}
		}
		p.clearBootErr(udid)
		p.reportBoot(udid)
		return nil
	}
	if err := p.GateBoot(ctx, udid); err != nil {
		return err
	}
	if err := p.reg.Boot(ctx, udid); err != nil {
		return err
	}
	if err := p.waitBooted(ctx, udid); err != nil {
		return err
	}
	// This synchronous path (Add/rePark/Recycle) brought the sim up:
	// a failure recorded by an earlier BootAsync no longer applies and
	// must not refuse a later client boot.
	p.clearBootErr(udid)
	p.reportBoot(udid)
	return nil
}

func (p *Pool) waitBooted(ctx context.Context, udid string) error {
	deadline := time.Now().Add(bootWait)
	for {
		st, err := p.reg.Health(ctx, udid)
		if err == nil && st == proto.StateBooted {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("boot %s: not Booted after %s (state %v, err %v)", udid, bootWait, st, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(bootPoll):
		}
	}
}

// Add brings a simulator into the pool: boot (gated) -> slim -> park.
// Membership is recorded only after the first successful park so the
// footprint watchdog never recycles a sim that is still slimming/booting.
func (p *Pool) Add(ctx context.Context, udid string) (err error) {
	release, takeover, ok := p.reserveTarget(udid)
	if !ok {
		return fmt.Errorf("pool add %s: target is leased", udid)
	}
	// A failed adoption of a FREE target never quarantines: either the
	// sim was untouched (gate refusal) or it became a member below and
	// the watchdog backstop repairs it. A quarantine takeover, however,
	// is only released as rebuilt after the erase below succeeded — the
	// previous holder's data must never reach the next lease.
	defer func() { release(!takeover || err == nil) }()
	defer p.BeginTransition(udid)()
	// An already-parked target doesn't add to the parked count: a retry
	// of an Add whose previous attempt parked the tree but failed on the
	// way out must not be refused against its own cap slot.
	if n := p.prk.ParkedCount(); n >= p.cfg.Class.MaxParked && !p.prk.IsParked(udid) {
		return fmt.Errorf("%w: %d parked >= cap %d", ErrPoolFull, n, p.cfg.Class.MaxParked)
	}
	if takeover {
		// The hold displaced a failed-reset quarantine: wipe before
		// rebuilding (thaw first — shutdown-while-parked wedges ~34s).
		if err = p.prk.Thaw(udid); err != nil {
			return fmt.Errorf("pool add %s: thaw: %w", udid, err)
		}
		if t, gerr := p.reg.Get(ctx, udid); gerr == nil && t.State == proto.StateBooted {
			if err = p.reg.Shutdown(ctx, udid); err != nil {
				return fmt.Errorf("pool add %s: shutdown: %w", udid, err)
			}
			p.notifyShutdown(udid)
		}
		if err = p.cfg.Erase(ctx, udid); err != nil {
			return fmt.Errorf("pool add %s: erase: %w", udid, err)
		}
	}
	if err = p.bootGated(ctx, udid); err != nil {
		return err
	}
	// Membership is recorded as soon as the boot succeeds: if slim/park
	// fail below (e.g. the adoption context expiring mid-slim), the
	// watchdog backstop re-parks or re-provisions the member instead of
	// leaving an unmanaged booted sim consuming a running slot. The
	// transition marker above keeps the watchdog away until Add returns.
	p.mu.Lock()
	p.members[udid] = true
	// Membership supersedes any earlier client-boot ownership record:
	// the watchdog owns members, so the janitor must never reclaim one.
	delete(p.daemonBooted, udid)
	delete(p.idleSince, udid)
	p.mu.Unlock()
	if p.cfg.Slim != nil {
		if serr := p.cfg.Slim(ctx, udid); serr != nil {
			p.log.Warn("pool slim failed; parking unslimmed", "udid", udid, "err", serr)
		}
	}
	_, err = p.parkIdle(ctx, udid)
	return err
}

// OnGrant thaws a pool sim the moment a lease is granted on it. Callers
// wire it into the lease manager's event sink. Safe for non-members and
// un-parked sims (no-op).
func (p *Pool) OnGrant(udid string) {
	if udid == "" {
		return
	}
	p.mu.Lock()
	p.grantSeq[udid]++
	delete(p.awakeUntil, udid) // the lease lifecycle owns the sim now
	p.mu.Unlock()
	if !p.prk.IsParked(udid) {
		return
	}
	start := time.Now()
	if err := p.prk.Thaw(udid); err != nil {
		p.log.Error("pool thaw failed", "udid", udid, "err", err)
		return
	}
	p.log.Info("pool sim thawed for lease", "udid", udid, "thaw_ms", time.Since(start).Milliseconds())
}

// EnsureThawed wakes a parked pool sim before destructive out-of-band
// work (e.g. a pre-grant erase, where no lease was granted so OnGrant
// never fired): simctl shutdown against a SIGSTOPped tree wedges ~34s.
// No-op for non-members and un-parked sims.
func (p *Pool) EnsureThawed(udid string) error {
	if udid == "" || !p.prk.IsParked(udid) {
		return nil
	}
	start := time.Now()
	if err := p.prk.Thaw(udid); err != nil {
		p.log.Error("pool thaw failed", "udid", udid, "err", err)
		return err
	}
	p.log.Info("pool sim thawed for reset", "udid", udid, "thaw_ms", time.Since(start).Milliseconds())
	return nil
}

// OnReleased re-parks a pool member after its post-lease reset (the reset
// already erased and rebooted or left it shut down; we ensure Booted then
// park). Non-members are ignored.
func (p *Pool) OnReleased(ctx context.Context, udid string) error {
	if !p.IsMember(udid) {
		return nil
	}
	defer p.BeginTransition(udid)()
	return p.rePark(ctx, udid)
}

// Recycle tears a pool sim down and rebuilds it: thaw -> shutdown -> reap
// orphans -> erase -> boot -> slim -> park. Used by the footprint
// watchdog when a sim's memory runs away.
func (p *Pool) Recycle(ctx context.Context, udid string) (err error) {
	// Hold the target in the lease manager for the whole teardown so a
	// lease can't be granted on a sim that is being shut down and wiped;
	// a failed rebuild keeps it quarantined until a later sweep succeeds.
	release, takeover, ok := p.reserveTarget(udid)
	if !ok {
		return fmt.Errorf("recycle %s: target is leased", udid)
	}
	if p.isRecording(udid) {
		// A lease-end stop may still be finalizing the mp4 for a few
		// seconds after the lease is gone; shutting the sim down now
		// would hang the recordVideo child. A later sweep retries.
		// Nothing was touched: a plain free target goes straight back
		// to leasable, while a displaced quarantine (takeover) must
		// stay quarantined — it was never erased.
		release(!takeover)
		return fmt.Errorf("recycle %s: recording is finalizing", udid)
	}
	defer func() { release(err == nil) }()
	// Runs before the release above (LIFO): a successful recycle erased
	// the sim, so clear its dirty mark before the hold is dropped and
	// queued leases are promoted onto it.
	defer func() {
		if err == nil {
			p.notifyClean(udid)
		}
	}()
	defer p.BeginTransition(udid)()
	// ALWAYS thaw before shutdown (parked shutdown wedges ~34s); a
	// failed thaw must abort rather than wedge the shutdown below.
	if err := p.prk.Thaw(udid); err != nil {
		return fmt.Errorf("recycle %s: thaw: %w", udid, err)
	}
	if err := p.reg.Shutdown(ctx, udid); err != nil {
		return fmt.Errorf("recycle %s: shutdown: %w", udid, err)
	}
	p.notifyShutdown(udid)
	p.reportShutdown(udid, "watchdog", "footprint recycle (erase + rebuild)")
	if err := p.ReapOrphans(ctx, udid); err != nil {
		p.log.Warn("orphan reap failed", "udid", udid, "err", err)
	}
	if err := p.cfg.Erase(ctx, udid); err != nil {
		return fmt.Errorf("recycle %s: erase: %w", udid, err)
	}
	return p.rePark(ctx, udid)
}

// rePark boots (gated), re-slims, and parks a pool sim. A shut-down sim
// is reaped first so leftover tree processes from the previous cycle
// (e.g. a reset whose simctl shutdown didn't take everything down)
// don't accumulate across generations.
func (p *Pool) rePark(ctx context.Context, udid string) error {
	// A rebuild supersedes any operator wake grace: otherwise the park
	// below is thawed straight back and the sim burns CPU un-leased.
	p.clearAwake(udid)
	// It also supersedes an intentional shutdown: the sim is being
	// rebuilt into the pool, so it must rejoin the watchdog's care.
	p.clearDown(udid)
	if t, err := p.reg.Get(ctx, udid); err == nil && t.State != proto.StateBooted {
		if err := p.ReapOrphans(ctx, udid); err != nil {
			p.log.Warn("pre-boot orphan reap failed", "udid", udid, "err", err)
		}
	}
	if err := p.bootGated(ctx, udid); err != nil {
		return fmt.Errorf("pool re-boot %s: %w", udid, err)
	}
	if p.cfg.Slim != nil {
		if err := p.cfg.Slim(ctx, udid); err != nil {
			p.log.Warn("pool re-slim failed; parking unslimmed", "udid", udid, "err", err)
		}
	}
	_, err := p.parkIdle(ctx, udid)
	return err
}

// SafeShutdown thaws first (never shut a parked sim down: it wedges
// ~34s), then shuts down and reaps orphans. The transition marker keeps
// the watchdog from re-parking the sim mid-shutdown.
func (p *Pool) SafeShutdown(ctx context.Context, udid string) error {
	defer p.BeginTransition(udid)()
	if err := p.prk.Thaw(udid); err != nil {
		// Abort: shutting down a (partly) frozen tree wedges ~34s, and
		// dropping membership below would strand the parked ledger entry
		// past any watchdog recovery, leaving Boot/Health lying forever.
		return fmt.Errorf("safe shutdown %s: thaw: %w", udid, err)
	}
	if err := p.reg.Shutdown(ctx, udid); err != nil {
		return err
	}
	p.notifyShutdown(udid)
	if err := p.ReapOrphans(ctx, udid); err != nil {
		p.log.Warn("orphan reap failed", "udid", udid, "err", err)
	}
	// An externally requested shutdown must stick: mark the member
	// intentionally down so the watchdog's "rebuild shut-down member"
	// branch doesn't boot the sim right back up. Membership is kept —
	// only Pool.Add (startup) records members, so dropping it here would
	// permanently shrink the pool: OnReleased would stop re-parking the
	// sim after its next lease and the watchdog would stop sweeping it.
	// The marker clears on an explicit boot or the next rePark.
	// (Recycle's internal shutdown goes through the registry directly
	// and never marks down.)
	p.mu.Lock()
	if p.members[udid] {
		p.down[udid] = true
	}
	delete(p.awakeUntil, udid)
	delete(p.daemonBooted, udid)
	delete(p.idleSince, udid)
	p.mu.Unlock()
	return nil
}

// RecoverFrozenParks thaws pool sims left SIGSTOPped by a previous
// daemon that exited uncleanly (crash/SIGKILL: Close's ThawAll never ran
// and the in-memory park ledger died with the process). Run once at
// startup before provisioning: a stranded frozen tree hangs every client
// action against the sim and stalls Pool.Add's slim step (simctl spawn
// against a stopped tree blocks) until the provisioning timeout. Only
// the configured pool UDIDs' trees are touched — never another agent's
// sims, which may be parked by a different daemon on purpose.
func (p *Pool) RecoverFrozenParks(ctx context.Context, udids []string) {
	procs, err := p.host.Processes(ctx)
	if err != nil {
		p.log.Warn("frozen-park recovery: process listing failed", "err", err)
		return
	}
	for _, udid := range udids {
		if udid = strings.TrimSpace(udid); udid == "" {
			continue
		}
		frozen := stoppedPIDs(procs, simTree(procs, udid))
		if len(frozen) == 0 {
			continue
		}
		p.log.Warn("thawing frozen sim tree left by a previous daemon", "udid", udid, "pids", frozen)
		for _, pid := range frozen {
			if err := p.host.Signal(pid, syscall.SIGCONT); err != nil {
				p.log.Warn("frozen-park recovery: SIGCONT failed", "udid", udid, "pid", pid, "err", err)
			}
		}
	}
}

// ReapOrphans kills leftover launchd_sim-tree processes for a UDID after
// shutdown/delete — but ONLY PIDs recorded in this daemon's own park
// ledger, never another agent's trees. Survivors get SIGCONT (a stopped
// process ignores SIGKILL delivery ordering otherwise) then SIGKILL.
func (p *Pool) ReapOrphans(ctx context.Context, udid string) error {
	owned := p.prk.OwnedTree(udid)
	if len(owned) == 0 {
		return nil
	}
	procs, err := p.host.Processes(ctx)
	if err != nil {
		return err
	}
	live := liveOwned(procs, owned)
	if len(live) == 0 {
		p.prk.ForgetOwned(udid)
		return nil
	}
	p.log.Info("reaping orphaned simulator processes", "udid", udid, "pids", live)
	var firstErr error
	for _, pid := range live {
		_ = p.host.Signal(pid, syscall.SIGCONT) // un-wedge stopped orphans first
		if err := p.host.Signal(pid, syscall.SIGKILL); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		p.prk.ForgetOwned(udid)
	}
	return firstErr
}

func (p *Pool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Close thaws everything parked so a daemon restart never strands
// SIGSTOPped trees. It first marks the pool closed so no new park can
// start, then joins in-flight parks (each is only a ps-walk plus
// signals, i.e. seconds) so a SIGSTOP mid-delivery cannot land after
// the final ThawAll and be stranded by process exit.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	for p.parking > 0 {
		p.parkDone.Wait()
	}
	p.mu.Unlock()
	p.prk.ThawAll()
}
