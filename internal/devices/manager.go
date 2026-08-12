package devices

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions"
	"github.com/BariBariGood/manzanas/internal/actions/wda"
	"github.com/BariBariGood/manzanas/proto"
)

// Manager applies a DevicesConfig to a running daemon: it toggles device
// enumeration, rewires the device backend's WDA clients, and owns the
// per-device runner and forward supervisors, starting and stopping them
// as configs come and go — so attaching a phone is a config write (or an
// admin POST), never a daemon restart.
type Manager struct {
	log     *slog.Logger
	backend *actions.DeviceBackend
	// setEnabled toggles device enumeration (registry toggle + the
	// server's device-protection guards); it also receives the config's
	// device UDIDs so the action router can pin them as devices.
	setEnabled func(enabled bool, deviceUDIDs []string)

	mu   sync.Mutex
	sups map[string]*deviceSup
	// cur is guarded by curMu, not mu, so Current (GET /v0/devices) stays
	// responsive while a convergence holds mu through supervisor teardown
	// waits.
	curMu sync.Mutex
	cur   proto.DevicesConfig
	// wired tracks the config each device's current WDA client in the
	// backend was built from (the effective, enabled set — not cur.WDA,
	// which is also remembered while enabled=false). Rewiring a client
	// discards its session and restart-recovery state, so an unchanged
	// config must keep the existing client.
	wired map[string]proto.DeviceWDAConfig
	// wiredMirror tracks the socket each device's current mirror client
	// in the backend was built from (at most one: the mirror is exclusive
	// global state). Rewiring replaces the client — and the mutex that
	// serializes gestures on the exclusive mirror window — so an
	// unchanged socket must keep the existing client.
	wiredMirror map[string]string
}

// deviceSup is one device's running supervision: the runner and/or
// forward supervisors started for its current config.
type deviceSup struct {
	cfg    proto.DeviceWDAConfig
	cancel context.CancelFunc
	wg     sync.WaitGroup
	kick   func()
}

// NewManager builds a manager. backend may be nil (mock daemons: only the
// enable toggle applies). setEnabled must not be nil.
func NewManager(backend *actions.DeviceBackend, setEnabled func(enabled bool, deviceUDIDs []string), log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log, backend: backend, setEnabled: setEnabled,
		sups: map[string]*deviceSup{}, wired: map[string]proto.DeviceWDAConfig{}, wiredMirror: map[string]string{}}
}

// Current returns the last applied config (deep-copied).
func (m *Manager) Current() proto.DevicesConfig {
	m.curMu.Lock()
	defer m.curMu.Unlock()
	return copyConfig(m.cur)
}

// Apply validates cfg and converges the running daemon onto it: devices
// removed from the config lose their supervisors and WDA clients, changed
// ones are restarted, added ones are started. Validation failures leave
// the running state untouched.
func (m *Manager) Apply(cfg proto.DevicesConfig) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	cfg = copyConfig(cfg)
	m.mu.Lock()
	defer m.mu.Unlock()
	// Enabled gates the whole device surface, matching the old --devices
	// semantics where WDA flags without --devices were inert: with
	// enabled=false no supervisor runs and no WDA client is wired, but
	// the config is remembered (Current reports it; re-enabling starts
	// everything back up).
	effective := cfg.WDA
	effectiveMirror := cfg.Mirror
	if !cfg.Enabled {
		effective = nil
		effectiveMirror = nil
	}
	// Stop supervisors for removed or changed devices first so a
	// replacement never races its predecessor for the phone or the port.
	for udid, sup := range m.sups {
		nd, ok := effective[udid]
		if ok && nd == sup.cfg {
			continue
		}
		// Wait for the predecessor to actually die before a replacement
		// starts: two xcodebuild sessions racing for the phone (or two
		// forwarders for the port) is worse than a slow config change.
		// The wait is bounded (m.mu is held, so an unkillable child must
		// not wedge Close/SIGHUP forever; Current reads under its own
		// lock and stays responsive throughout) — on timeout the change
		// proceeds with a loud error rather than deadlocking the daemon.
		if !sup.stopWait(m.log, udid) {
			m.log.Error("device supervisor did not stop; replacement may race its leftover child", "udid", udid)
		}
		delete(m.sups, udid)
	}
	if m.backend != nil {
		for udid := range m.wired {
			if _, ok := effective[udid]; !ok {
				m.backend.SetWDA(udid, "", nil)
				delete(m.wired, udid)
				m.log.Info("device WDA removed", "udid", udid)
			}
		}
		for udid := range m.wiredMirror {
			if _, ok := effectiveMirror[udid]; !ok {
				m.backend.SetMirror(udid, "")
				delete(m.wiredMirror, udid)
				m.log.Info("device mirror backend removed", "udid", udid)
			}
		}
		for udid, d := range effectiveMirror {
			socket := d.Socket
			if socket == "" {
				socket = DefaultMirrorSocket()
			}
			if old, ok := m.wiredMirror[udid]; ok && old == socket {
				continue // unchanged, client keeps running
			}
			m.backend.SetMirror(udid, socket)
			m.wiredMirror[udid] = socket
			m.log.Info("device mirror backend configured", "udid", udid, "socket", socket)
		}
	}
	for _, udid := range UDIDs(proto.DevicesConfig{WDA: effective}) {
		d := effective[udid]
		if old, ok := m.wired[udid]; ok && old == d {
			continue // unchanged, client and supervisors keep running
		}
		if old, ok := m.sups[udid]; ok && old.cfg == d {
			continue // unchanged, supervisors keep running
		}
		sup := m.startSupervisors(udid, d)
		if sup != nil {
			m.sups[udid] = sup
		}
		if m.backend != nil {
			var kick func()
			if sup != nil {
				kick = sup.kick
			}
			m.backend.SetWDA(udid, d.URL, kick)
			m.wired[udid] = d
			m.log.Info("device WDA configured", "udid", udid, "url", d.URL,
				"launch", d.Launch, "forward", d.Forward)
		}
	}
	m.curMu.Lock()
	m.cur = cfg
	m.curMu.Unlock()
	m.setEnabled(cfg.Enabled, UDIDs(cfg))
	return nil
}

// startSupervisors starts the runner and/or forward supervisors for one
// device config; returns nil when the config needs none. Specs were
// validated by Apply, so parse errors cannot happen here.
func (m *Manager) startSupervisors(udid string, d proto.DeviceWDAConfig) *deviceSup {
	if d.Launch == "" && d.Forward == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	sup := &deviceSup{cfg: d, cancel: cancel}
	var kicks []func()
	if d.Forward != "" {
		fwd, _ := wda.ParseForward(udid, d.Forward)
		s := wda.NewSupervisor(wda.New(d.URL), fwd, m.log, wda.WithHealthFunc(fwd.Healthy))
		kicks = append(kicks, s.Kick)
		sup.wg.Add(1)
		go func() {
			defer sup.wg.Done()
			s.Run(ctx)
		}()
		m.log.Info("device forward supervisor started", "udid", udid, "forward", fwd.String())
	}
	if d.Launch != "" {
		launcher, _ := wda.ParseLauncher(udid, d.Launch)
		s := wda.NewSupervisor(wda.New(d.URL), launcher, m.log)
		kicks = append(kicks, s.Kick)
		sup.wg.Add(1)
		go func() {
			defer sup.wg.Done()
			s.Run(ctx)
		}()
		m.log.Info("device WDA supervisor started", "udid", udid, "launcher", launcher.String())
	}
	sup.kick = func() {
		for _, k := range kicks {
			k()
		}
	}
	return sup
}

// stop cancels the device's supervisors and waits (bounded) for their
// launcher.Stop cleanup; used on daemon shutdown, where the process is
// exiting anyway and a hung child must not wedge the exit.
func (s *deviceSup) stop() {
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// stopWait cancels the device's supervisors and waits — well past the
// bounded shutdown stop, but not forever — for their launcher.Stop
// cleanup, logging while it drags on. Config changes use this so a
// replacement supervisor never races a still-dying predecessor for the
// phone or the forward port; it reports false if the wait expired.
func (s *deviceSup) stopWait(log *slog.Logger, udid string) bool {
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-done:
			return true
		case <-deadline:
			return false
		case <-time.After(5 * time.Second):
			log.Warn("waiting for device supervisor to stop", "udid", udid)
		}
	}
}

// Close stops every supervisor (daemon shutdown).
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for udid, sup := range m.sups {
		sup.stop()
		delete(m.sups, udid)
	}
}

func copyConfig(cfg proto.DevicesConfig) proto.DevicesConfig {
	out := proto.DevicesConfig{Enabled: cfg.Enabled}
	if cfg.WDA != nil {
		out.WDA = make(map[string]proto.DeviceWDAConfig, len(cfg.WDA))
		for u, d := range cfg.WDA {
			out.WDA[u] = d
		}
	}
	if cfg.Mirror != nil {
		out.Mirror = make(map[string]proto.DeviceMirrorConfig, len(cfg.Mirror))
		for u, d := range cfg.Mirror {
			out.Mirror[u] = d
		}
	}
	return out
}
