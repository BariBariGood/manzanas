// Command manzanasd is the fleet daemon: it enumerates simulator targets,
// arbitrates leases, and serves the v0 protocol over HTTP + WebSocket.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions"
	"github.com/BariBariGood/manzanas/internal/actions/wda"
	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/record"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/internal/server"
	"github.com/BariBariGood/manzanas/internal/state"
	"github.com/BariBariGood/manzanas/internal/stream"
	"github.com/BariBariGood/manzanas/internal/warm"
	"github.com/BariBariGood/manzanas/proto"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	v, _ := strconv.ParseBool(os.Getenv(key))
	return v
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// defaultJournalDir is ~/.manzanasd/journal (empty when home is unknown,
// which disables the journal).
func defaultJournalDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".manzanasd", "journal")
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".manzanasd"
	}
	return filepath.Join(home, ".manzanasd")
}

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

// parseDeviceWDA parses the --device-wda flag: comma-separated
// "<udid>=<url>" pairs mapping a device to its WebDriverAgent endpoint.
func parseDeviceWDA(spec string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		udid, url, ok := strings.Cut(pair, "=")
		if !ok || udid == "" || url == "" {
			return nil, fmt.Errorf("invalid --device-wda entry %q (want <udid>=<url>)", pair)
		}
		out[udid] = url
	}
	return out, nil
}

// provisionPool brings the configured sims into the pool, retrying failed
// adds with capped exponential backoff: startup adds race the load spike
// of whatever launched the daemon (a stamp, a build), and a one-shot add
// would leave the pool permanently empty until a manual restart. Unknown
// UDIDs are dropped (retrying cannot fix a typo); everything else retries
// until it succeeds or the daemon exits.
func provisionPool(ctx context.Context, pool *warm.Pool, udids []string, log *slog.Logger) {
	const (
		initialBackoff = 30 * time.Second
		maxBackoff     = 5 * time.Minute
	)
	pending := make([]string, 0, len(udids))
	for _, u := range udids {
		if u = strings.TrimSpace(u); u != "" {
			pending = append(pending, u)
		}
	}
	backoff := initialBackoff
	for len(pending) > 0 {
		var retry []string
		for _, u := range pending {
			actx, cancel := context.WithTimeout(ctx, 15*time.Minute)
			err := pool.Add(actx, u)
			cancel()
			var nf *registry.NotFoundError
			switch {
			case err == nil:
				log.Info("pool sim parked", "udid", u)
			case errors.As(err, &nf):
				log.Error("pool add failed: unknown target; dropping", "udid", u, "err", err)
			default:
				log.Warn("pool add failed; will retry", "udid", u, "backoff", backoff, "err", err)
				retry = append(retry, u)
			}
		}
		pending = retry
		if len(pending) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func main() {
	var (
		addr            = flag.String("addr", envOr("MANZANASD_ADDR", ":7433"), "listen address (env MANZANASD_ADDR)")
		mock            = flag.Bool("mock", envBool("MANZANASD_MOCK"), "use the mock registry instead of simctl (env MANZANASD_MOCK)")
		devices         = flag.Bool("devices", envBool("MANZANASD_DEVICES"), "also enumerate physical devices via devicectl as leasable targets (env MANZANASD_DEVICES)")
		deviceWDA       = flag.String("device-wda", envOr("MANZANASD_DEVICE_WDA", ""), "comma-separated <udid>=<url> WebDriverAgent endpoints for device HID/observe/screenshot (env MANZANASD_DEVICE_WDA)")
		deviceWDALaunch = flag.String("device-wda-launch", envOr("MANZANASD_DEVICE_WDA_LAUNCH", ""), "comma-separated <udid>=<spec> WDA auto-launch specs: devicectl:<runner-bundle-id> or xctestrun:<path>; requires a matching --device-wda entry (env MANZANASD_DEVICE_WDA_LAUNCH)")
		stateDir        = flag.String("state-dir", envOr("MANZANASD_STATE_DIR", defaultStateDir()), "directory for the snapshot index (env MANZANASD_STATE_DIR)")
		axePath         = flag.String("axe", envOr("MANZANASD_AXE", ""), "path to the AXe binary (env MANZANASD_AXE; default: ~/bin/axe or $PATH)")
		simbridge       = flag.String("simbridge", envOr("MANZANASD_SIMBRIDGE", ""), "path to the simbridge warm-action helper (env MANZANASD_SIMBRIDGE; default: ~/bin/simbridge or $PATH)")
		warmOff         = flag.Bool("no-warm", envBool("MANZANASD_NO_WARM"), "disable the warm actions backend even when simbridge is present (env MANZANASD_NO_WARM)")
		warmMax         = flag.Int("warm-max-targets", actions.DefaultMaxWarmTargets, "max simulators kept warm at once")
		warmTTL         = flag.Duration("warm-idle-ttl", actions.DefaultWarmIdleTTL, "shut a warm helper down after this much inactivity")
		showVersion     = flag.Bool("version", false, "print version and exit")
		leaseGrace      = flag.Duration("lease-renew-grace", envDuration("MANZANASD_LEASE_RENEW_GRACE", lease.DefaultRenewGrace), "renewal grace window after an active lease's nominal expiry during which renew still succeeds and the reset/reclaim is deferred; 0 disables (env MANZANASD_LEASE_RENEW_GRACE)")

		poolSims          = flag.String("pool-sims", envOr("MANZANASD_POOL_SIMS", ""), "comma-separated sim UDIDs to keep in the park/thaw warm pool (env MANZANASD_POOL_SIMS)")
		poolSlimProfile   = flag.String("pool-slim-profile", envOr("MANZANASD_POOL_SLIM_PROFILE", ""), "simslim profile applied to pool sims before parking (env MANZANASD_POOL_SLIM_PROFILE)")
		poolFootprintCap  = flag.Int64("pool-footprint-cap-mb", envInt64("MANZANASD_POOL_FOOTPRINT_CAP_MB", 0), "phys_footprint (MiB) above which the watchdog recycles a pool sim; 0 = default 3072, negative disables the watchdog (env MANZANASD_POOL_FOOTPRINT_CAP_MB)")
		poolWatchdogEvery = flag.Duration("pool-watchdog-interval", envDuration("MANZANASD_POOL_WATCHDOG_INTERVAL", 0), "how often the footprint watchdog sweeps pool sims; 0 = default 1m (env MANZANASD_POOL_WATCHDOG_INTERVAL)")
		poolMaxRunning    = flag.Int("pool-max-running", int(envInt64("MANZANASD_POOL_MAX_RUNNING", 0)), "cap on Booted un-parked sims; 0 = capacity-class default (2 Intel / 4 Apple Silicon), negative disables the cap (env MANZANASD_POOL_MAX_RUNNING)")
		poolLoadFactor    = flag.Float64("pool-load-factor", envFloat("MANZANASD_POOL_LOAD_FACTOR", 0), "refuse sim boots when 1-min load > factor x cores; 0 = default 2.0, negative disables the load gate (env MANZANASD_POOL_LOAD_FACTOR)")
		poolMinFreeDisk   = flag.Int64("pool-min-free-disk-gb", envInt64("MANZANASD_POOL_MIN_FREE_DISK_GB", 0), "refuse sim boots below this much free disk (GiB); 0 = default 5, negative disables the disk gate (env MANZANASD_POOL_MIN_FREE_DISK_GB)")
		janitorReapLocks  = flag.Bool("janitor-reap-stale-locks", envBool("MANZANASD_JANITOR_REAP_STALE_LOCKS"), "let the janitor shut down (never delete) unmanaged Booted sims whose fleet lock file is stale (>=2h) or missing, after the staleness persists a full grace; off by default — recommended ON for fleet daemons (env MANZANASD_JANITOR_REAP_STALE_LOCKS)")
		lockDir           = flag.String("lock-dir", envOr("MANZANAS_LOCK_DIR", warm.DefaultLockDir), "fleet lock directory holding per-resource lock files like sim-<udid> (env MANZANAS_LOCK_DIR)")
		janitorEvery      = flag.Duration("janitor-interval", envDuration("MANZANASD_JANITOR_INTERVAL", 0), "how often the janitor reclaims idle daemon-booted sims and reports unmanaged ones; 0 = default 1m, negative disables (env MANZANASD_JANITOR_INTERVAL)")

		journalDir      = flag.String("journal-dir", envOr("MANZANASD_JOURNAL_DIR", defaultJournalDir()), "run journal directory; empty disables the journal (env MANZANASD_JOURNAL_DIR)")
		journalMaxAge   = flag.Duration("journal-max-age", envDuration("MANZANASD_JOURNAL_MAX_AGE", 7*24*time.Hour), "GC journal runs older than this; 0 disables (env MANZANASD_JOURNAL_MAX_AGE)")
		journalMaxBytes = flag.Int64("journal-max-bytes", envInt64("MANZANASD_JOURNAL_MAX_BYTES", 2<<30), "GC oldest journal runs above this total size; 0 disables (env MANZANASD_JOURNAL_MAX_BYTES)")

		recordMaxSeconds    = flag.Int("record-max-seconds", int(envInt64("MANZANASD_RECORD_MAX_SECONDS", record.DefaultMaxSeconds)), "hard duration cap per video recording (env MANZANASD_RECORD_MAX_SECONDS)")
		recordMaxBytes      = flag.Int64("record-max-bytes", envInt64("MANZANASD_RECORD_MAX_BYTES", record.DefaultMaxBytes), "hard size cap per video recording (env MANZANASD_RECORD_MAX_BYTES)")
		recordMaxConcurrent = flag.Int("record-max-concurrent", int(envInt64("MANZANASD_RECORD_MAX_CONCURRENT", record.DefaultMaxConcurrent)), "max simultaneous video recordings (env MANZANASD_RECORD_MAX_CONCURRENT)")

		streamMaxStreams = flag.Int("stream-max-streams", stream.DefaultMaxStreams, "max concurrently open streams")
		streamMaxViewers = flag.Int("stream-max-viewers", stream.DefaultMaxViewers, "max concurrent viewers per stream")
		streamFPS        = flag.Int("stream-fps", stream.DefaultFPS, "default stream capture rate (frames/sec)")
		streamMaxFPS     = flag.Int("stream-max-fps", stream.DefaultMaxFPS, "max stream capture rate (frames/sec)")
		streamLinger     = flag.Duration("stream-linger", stream.DefaultLinger, "how long a stream keeps capturing after its last viewer leaves")

		dashReadonly = flag.Bool("dash-readonly", envBool("MANZANASD_DASH_READONLY"), "disable the dashboard's mutating controls (boot/shutdown/release/stop-recording); the rest of the v0 API is unaffected (env MANZANASD_DASH_READONLY)")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("manzanasd %s (protocol %s)\n", version, proto.Version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Bind before any stateful setup (pool adoption SIGSTOPs sims, the
	// journal opens runs): a taken port must fail fast, not after side
	// effects — and never after a misleading "listening" log line.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var reg registry.Registry
	switch {
	case *mock:
		log.Info("using mock registry")
		reg = registry.NewMock()
	case runtime.GOOS != "darwin":
		log.Warn("not on macOS; falling back to mock registry (pass --mock to silence)")
		reg = registry.NewMock()
	default:
		reg = registry.NewSimctl()
		if *devices {
			// Physical devices join the same registry (and thus lease
			// matching) behind the merged view; they are excluded from the
			// warm pool, state engine, and streaming by construction (pool
			// membership is explicit UDIDs; state/stream backends are
			// simctl-based).
			reg = registry.Merge(reg, registry.NewDevicectl())
			log.Info("physical device targets enabled (devicectl)")
		}
	}

	// The state engine + image store come first so the pool's erase path
	// can share the engine's post-erase re-apply hook.
	var eng *state.SimEngine
	var imgs *state.ImageStore
	if runtime.GOOS == "darwin" && !*mock {
		hostRun := state.NewHostRunner("")
		eng = state.NewSimEngine(hostRun, filepath.Join(*stateDir, "snapshots.json"))
		slim := state.HostSlimFunc()
		if slim == nil {
			log.Warn("simslim not found; golden-image builds with a slim_profile will be refused")
		}
		imgs = state.NewImageStore(hostRun, filepath.Join(*stateDir, "images"), slim)
		imgs.SetSlimCheck(state.HostSlimProfileCheck)
		// Prefer `simslim verify` (exact profile-match with drift listing)
		// when the installed simslim has it; older binaries fall back to
		// the launchctl print-disabled parsing built into the store.
		if verify := state.HostSlimVerifyFunc(); verify != nil {
			imgs.SetSlimVerify(verify)
			log.Info("simslim verify available; using exact profile-match checks")
		} else if slim != nil {
			log.Info("installed simslim predates `verify`; using launchctl-based slim checks")
		}
		// simctl erase wipes the per-UDID launchctl disable config, so a
		// sim stamped from a slim image would come back stock; re-apply
		// its recorded disable set after every erase.
		eng.SetPostErase(func(ctx context.Context, udid string) error {
			_, err := imgs.ReapplySlim(ctx, udid)
			return err
		})
	}

	// The park/thaw pool and its safety gates (capacity classes, load gate,
	// free-space gate, thaw-before-shutdown) wrap the registry on real hosts.
	var pool *warm.Pool
	if runtime.GOOS == "darwin" && !*mock {
		var poolSlim warm.SlimFunc
		// An empty profile means "do not slim" (matching ImageStore.Build);
		// pass "default" to request simslim's built-in profile.
		if hostSlim := state.HostSlimFunc(); hostSlim != nil && *poolSlimProfile != "" {
			profile := *poolSlimProfile
			poolSlim = func(ctx context.Context, udid string) error {
				return hostSlim(ctx, udid, profile)
			}
			// Advisory: a profile that kills the speech daemons makes
			// per-keystroke typing wedge apps on iOS 26.5 (#99).
			if warnMsg, err := state.SlimProfileKeyboardWarning(profile); err != nil {
				log.Warn("pool slim profile check failed", "profile", profile, "err", err)
			} else if warnMsg != "" {
				log.Warn(warnMsg)
			}
		}
		// Pool erases go through the engine so a stamped-slim sim gets its
		// recorded disables re-applied even when --pool-slim-profile is
		// unset (raw `simctl erase` would bring it back stock while
		// slimmed.json still records it as slim).
		poolCfg := warm.Config{Slim: poolSlim, Erase: eng.Erase, Logger: log,
			ReapStaleLocks: *janitorReapLocks, LockDir: *lockDir}
		if *poolMaxRunning != 0 {
			poolCfg.Class = warm.DetectClass()
			poolCfg.Class.MaxBootedRunning = *poolMaxRunning
		}
		poolCfg.LoadFactor = *poolLoadFactor
		poolCfg.MinFreeDisk = *poolMinFreeDisk * (1 << 30)
		pool = warm.NewPool(warm.NewOSHost(), reg, poolCfg)
		defer pool.Close() // thaw everything: never strand SIGSTOPped trees
		reg = warm.Guard(reg, pool)
		c := pool.Class()
		log.Info("warm pool gates active", "max_running", c.MaxBootedRunning,
			"max_parked", c.MaxParked, "max_concurrent_boots", c.MaxConcurrentBoots,
			"load_factor", pool.LoadFactor(),
			"min_free_disk_gb", float64(pool.MinFreeDisk())/(1<<30))
	}

	srv := server.New(reg, nil, log)
	srv.SetDashReadonly(*dashReadonly)
	if pool != nil {
		// Streams refuse parked pool sims: frame capture against a
		// SIGSTOPped tree hangs; a lease thaws them.
		srv.SetParkedCheck(pool.Parker().IsParked)
		// Explicit lease releases reclaim daemon-booted non-pool sims
		// (expiry reaches the pool through the lease event sink). Off
		// the request path: the reclaim shells out to simctl, and if
		// the post-lease reset holds the target the periodic janitor
		// picks the sim up later.
		srv.SetLeaseEndHook(func(udid string) { go pool.OnLeaseEnd(udid) })
		srv.SetPoolStatus(pool.Status)
	}
	if eng != nil {
		srv.SetState(eng)
		srv.SetImages(imgs)
	}

	var actOpts []actions.Option
	if *axePath != "" {
		actOpts = append(actOpts, actions.WithAXePath(*axePath))
	}
	cold := actions.NewAXe(actOpts...)
	if cold.AXeAvailable() {
		log.Info("actions backend ready", "axe", cold.AXePath())
	} else {
		log.Warn("AXe not found; HID and observe actions will return 'unavailable' (install github.com/cameroncooke/AXe at ~/bin/axe)")
	}
	var backend actions.Backend = cold
	var warmBackend *actions.WarmBackend
	if bridgePath := actions.DetectSimbridge(*simbridge); bridgePath != "" && !*warmOff {
		warmBackend = actions.NewWarm(cold,
			func(udid string) (actions.Helper, error) { return actions.NewProcHelper(bridgePath, udid) },
			actions.PoolConfig{MaxTargets: *warmMax, IdleTTL: *warmTTL, Logger: log})
		defer warmBackend.Close()
		backend = warmBackend
		log.Info("warm actions backend ready", "simbridge", bridgePath, "max_targets", *warmMax, "idle_ttl", *warmTTL)
	} else if !*warmOff {
		log.Info("simbridge not found; actions use the cold AXe path (build helpers/simbridge to enable warm actions)")
	}
	stopWDASupervisors := func() {}
	if *devices {
		wdaMap, err := parseDeviceWDA(*deviceWDA)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		launchMap, err := parseDeviceWDA(*deviceWDALaunch)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		// Each device with both a WDA URL and a launch spec gets a
		// supervisor that keeps its WebDriverAgent alive for the daemon's
		// lifetime (probe, launch when down, relaunch on failure).
		supCtx, supStop := context.WithCancel(context.Background())
		var supWG sync.WaitGroup
		// Validate every launch spec before starting any supervisor: a
		// bad entry must fail fast without leaving an already-launched
		// xcodebuild child behind (os.Exit skips the deferred cleanup).
		type devSup struct {
			udid     string
			url      string
			launcher wda.Launcher
		}
		var sups []devSup
		for udid, spec := range launchMap {
			url, ok := wdaMap[udid]
			if !ok {
				fmt.Fprintf(os.Stderr, "--device-wda-launch %s=%s has no matching --device-wda URL\n", udid, spec)
				os.Exit(1)
			}
			launcher, err := wda.ParseLauncher(udid, spec)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			sups = append(sups, devSup{udid: udid, url: url, launcher: launcher})
		}
		kicks := map[string]func(){}
		for _, d := range sups {
			sup := wda.NewSupervisor(wda.New(d.url), d.launcher, log)
			kicks[d.udid] = sup.Kick
			supWG.Add(1)
			go func() {
				defer supWG.Done()
				sup.Run(supCtx)
			}()
			log.Info("device WDA supervisor started", "udid", d.udid, "launcher", d.launcher.String())
		}
		// Supervisors own long-lived xcodebuild children; wait for their
		// launcher.Stop cleanup so a daemon exit never orphans a test
		// session holding the phone.
		var supOnce sync.Once
		stopWDASupervisors = func() {
			supOnce.Do(func() {
				supStop()
				done := make(chan struct{})
				go func() { supWG.Wait(); close(done) }()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					log.Warn("wda supervisors did not stop in time")
				}
			})
		}
		defer stopWDASupervisors()
		dev := actions.NewDevice(actions.WithDeviceWDA(wdaMap), actions.WithDeviceWDAKick(kicks))
		backend = actions.NewKindRouter(reg, backend, dev)
		srv.SetDevicesEnabled(true)
		for udid, u := range wdaMap {
			log.Info("device WDA endpoint configured", "udid", udid, "url", u)
		}
	}
	srv.SetActions(backend)

	// The journal is wired before the lease manager starts so its expiry
	// goroutine (which reads srv.journal via EventSink) never races the write.
	gcCtx, gcStop := context.WithCancel(context.Background())
	defer gcStop()
	if *journalDir != "" {
		store, err := journal.NewFileStore(*journalDir)
		if err != nil {
			log.Error("journal disabled", "err", err)
		} else {
			log.Info("journal enabled", "dir", *journalDir)
			srv.SetJournal(journal.NewRecorder(store))
			go store.RunGC(gcCtx, journal.GCConfig{MaxAge: *journalMaxAge, MaxBytes: *journalMaxBytes})

			// Video recording rides on the journal (spool + artifacts live
			// under the run dir), so it is only wired when the journal is.
			var starter record.Starter = record.SimctlStarter{}
			if *mock || runtime.GOOS != "darwin" {
				starter = record.MockStarter{}
			}
			recMgr := record.NewManager(record.Config{
				Starter:       starter,
				JournalRoot:   *journalDir,
				MaxSeconds:    *recordMaxSeconds,
				MaxBytes:      *recordMaxBytes,
				MaxConcurrent: *recordMaxConcurrent,
				Logger:        log,
			})
			srv.SetRecorderManager(recMgr)
			if pool != nil {
				// A pool recycle / adoption / SafeShutdown cycles the sim
				// through a shutdown, which recovers a wedged host
				// recording session — clear the poisoned flag then.
				pool.SetShutdownFunc(recMgr.ClearPoisoned)
				// The watchdog must never freeze (park) or tear down a sim
				// whose recording is still live or finalizing — the
				// lease-end stop runs asynchronously for a few seconds
				// after the lease is gone.
				pool.SetRecordingFunc(recMgr.Recording)
			}
			// Recorder children from a previous daemon run get a SIGINT and
			// their salvageable spools are ingested (reason daemon_restart);
			// pre-existing recordVideo strays are logged, never signaled.
			srv.RecoverRecordings()
			// Stop + ingest every live recording before the pool/registry
			// defers shut sims down (a recordVideo child hangs if its sim
			// goes away mid-recording). Deferred after pool creation, so it
			// runs before pool.Close on the LIFO unwind.
			defer srv.StopAllRecordings("daemon_shutdown")
			log.Info("video recording enabled", "max_seconds", *recordMaxSeconds,
				"max_bytes", *recordMaxBytes, "max_concurrent", *recordMaxConcurrent)
		}
	}

	sink := srv.EventSink()
	if pool != nil {
		inner := sink
		sink = func(l proto.Lease) {
			inner(l)
			// A lease reaching a terminal state frees its target: a
			// daemon-booted non-pool sim is shut down right away instead
			// of burning CPU until an operator notices. Leases with a
			// reset never get here dirty (the reset path re-parks pool
			// members; OnLeaseEnd ignores members entirely), and the
			// shutdown shells out, so it runs off the expiry loop.
			if l.State == proto.LeaseExpired && l.TargetUDID != "" {
				go pool.OnLeaseEnd(l.TargetUDID)
			}
		}
	}
	leases := lease.New(reg, sink)
	leases.SetRenewGrace(*leaseGrace)
	defer leases.Close()
	if pool != nil {
		// Thaw a parked pool sim the instant its lease becomes active
		// (cached-PID SIGCONT: ~25ms AS / ~225ms Intel).
		leases.SetOnActive(func(l proto.Lease) { pool.OnGrant(l.TargetUDID) })
		// Janitor/watchdog shutdown decisions land in the server's ledger
		// so a leaseholder that finds its target down learns who and why.
		pool.SetShutdownReporter(srv.NoteShutdown)
		// Pool boot paths (rePark, Recycle, BootAsync) bring sims up
		// without the server's boot handler: report them so stale
		// shutdown-ledger entries don't outlive the shutdown they record.
		pool.SetBootReporter(srv.NoteBoot)
		// Boot waits probe the cheap gates (no target listing) between
		// full attempts so waiting stays cheap on an overloaded host.
		srv.SetBootGates(pool.GateBootCheap)
		pool.SetMarkCleanFunc(leases.MarkClean)
		pool.SetLeasedFunc(func(udid string) bool {
			_, ok := leases.Active(udid)
			return ok
		})
		// Destructive pool transitions (recycle, adoption, re-provision)
		// hold the target in the lease manager so it can't be granted
		// mid-shutdown/erase/slim.
		pool.SetReserveFunc(func(udid string) (func(bool), bool, bool) {
			takeover, ok := leases.Reserve(udid)
			if !ok {
				return nil, false, false
			}
			return func(rebuilt bool) {
				if rebuilt {
					leases.Unreserve(udid)
				} else {
					leases.Quarantine(udid)
				}
			}, takeover, true
		})
	}
	resetSink := srv.ResetSink()
	resetFn := resetSink
	if warmBackend != nil {
		// A lease-end reset can erase/reboot the simulator underneath a
		// resident helper; drop it so the next action reconnects fresh.
		resetFn = func(l proto.Lease) error {
			warmBackend.Invalidate(l.TargetUDID)
			return resetSink(l)
		}
	}
	if pool != nil {
		inner := resetFn
		resetFn = func(l proto.Lease) error {
			// The whole reset+re-park sequence is one pool transition so
			// the footprint watchdog never parks a sim mid-erase/boot.
			defer pool.BeginTransition(l.TargetUDID)()
			// A pre-grant erase runs with no lease ever granted, so the
			// OnGrant thaw never fired: wake a still-parked member before
			// the destructive shutdown/erase (parked shutdown wedges).
			if err := pool.EnsureThawed(l.TargetUDID); err != nil {
				return err
			}
			if err := inner(l); err != nil {
				return err
			}
			// Pool members go back to parked after the post-lease reset:
			// erase -> boot -> slim -> park.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if err := pool.OnReleased(ctx, l.TargetUDID); err != nil {
				log.Error("pool re-park failed", "udid", l.TargetUDID, "err", err)
				// A Booted sim is clean and usable (only the park step
				// failed): hand it out; the watchdog re-parks it when
				// idle. Otherwise quarantine — the watchdog can take the
				// quarantined target over, rebuild it, and free it.
				if t, gerr := reg.Get(context.Background(), l.TargetUDID); gerr == nil && t.State == proto.StateBooted {
					return nil
				}
				return err
			}
			return nil
		}
	}
	leases.SetResetFunc(resetFn)
	srv.SetLeases(leases)

	if pool != nil {
		if *poolSims != "" {
			udids := strings.Split(*poolSims, ",")
			// Shield configured pool sims from the stale-lock reaper
			// while they await asynchronous adoption below.
			pool.SetConfiguredPool(udids)
			// A previous daemon that died uncleanly (Close never ran)
			// leaves pool trees SIGSTOPped with no ledger: thaw them
			// before provisioning or Add's slim step hangs against the
			// frozen tree and every client action on the sim stalls.
			pool.RecoverFrozenParks(context.Background(), udids)
			provisionCtx, provisionStop := context.WithCancel(context.Background())
			defer provisionStop()
			go provisionPool(provisionCtx, pool, udids, log)
		}
		if *poolFootprintCap < 0 {
			log.Info("footprint watchdog disabled", "pool-footprint-cap-mb", *poolFootprintCap)
		} else {
			stopWatchdog := pool.StartWatchdog(context.Background(), *poolWatchdogEvery, *poolFootprintCap<<10, func(udid string) bool {
				_, ok := leases.Active(udid)
				return ok
			})
			defer stopWatchdog()
		}
		if *janitorEvery < 0 {
			log.Info("janitor disabled", "janitor-interval", *janitorEvery)
		} else {
			interval := *janitorEvery
			if interval == 0 {
				interval = warm.DefaultJanitorInterval
			}
			stopJanitor := pool.StartJanitor(context.Background(), interval)
			defer stopJanitor()
			log.Info("janitor active", "interval", interval,
				"reap_stale_locks", *janitorReapLocks, "lock_dir", *lockDir)
		}
	}

	sourceFactory := stream.SimctlSourceFactory
	if *mock || runtime.GOOS != "darwin" {
		sourceFactory = stream.FakeSourceFactory
	}
	streamer := stream.NewManager(stream.Config{
		MaxStreams: *streamMaxStreams,
		MaxViewers: *streamMaxViewers,
		DefaultFPS: *streamFPS,
		MaxFPS:     *streamMaxFPS,
		Linger:     *streamLinger,
	}, sourceFactory, log)
	defer streamer.CloseAll()
	srv.SetStreamer(streamer)
	if pool != nil {
		// An idle re-park must not SIGSTOP a tree a viewer is capturing
		// frames from (streams need no lease).
		pool.SetStreamingFunc(streamer.HasStream)
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Info("manzanasd listening", "addr", ln.Addr().String(), "protocol", proto.Version)
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		// os.Exit skips deferred cleanup, so run it explicitly: stop and
		// ingest recordings first (a recordVideo child hangs if its sim
		// goes away mid-recording), then thaw the pool — the adoption
		// goroutine and watchdog may already have parked (SIGSTOPped)
		// sims that must never outlive the daemon frozen.
		srv.StopAllRecordings("daemon_shutdown")
		stopWDASupervisors()
		if pool != nil {
			pool.Close()
		}
		os.Exit(1)
	}
	stop()
	<-shutdownDone
}
