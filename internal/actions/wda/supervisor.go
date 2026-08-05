package wda

import (
	"context"
	"log/slog"
	"time"
)

// Supervisor defaults.
const (
	// DefaultProbeInterval is how often a healthy WDA is re-probed.
	DefaultProbeInterval = 10 * time.Second
	// DefaultReadyTimeout bounds how long one launch is given to come up.
	DefaultReadyTimeout = 90 * time.Second
	// defaultProbeTimeout bounds one GET /status round-trip.
	defaultProbeTimeout = 3 * time.Second
	// maxLaunchBackoff caps the delay between failed launch attempts.
	maxLaunchBackoff = 5 * time.Minute
)

// Supervisor keeps one device's WebDriverAgent alive: it probes the WDA
// endpoint, launches the runner when it is down (a dropped tunnel, a
// crashed runner, daemon startup), and keeps retrying with capped backoff.
// A device action that hits a dead WDA can Kick the supervisor to skip
// the probe interval.
type Supervisor struct {
	client   *Client
	launcher Launcher
	log      *slog.Logger

	interval     time.Duration
	readyTimeout time.Duration
	kick         chan struct{}
}

// SupervisorOption configures a Supervisor.
type SupervisorOption func(*Supervisor)

// WithProbeInterval sets the healthy re-probe interval.
func WithProbeInterval(d time.Duration) SupervisorOption {
	return func(s *Supervisor) { s.interval = d }
}

// WithReadyTimeout sets the per-launch readiness budget.
func WithReadyTimeout(d time.Duration) SupervisorOption {
	return func(s *Supervisor) { s.readyTimeout = d }
}

// NewSupervisor builds a supervisor for one device's WDA endpoint.
func NewSupervisor(client *Client, launcher Launcher, log *slog.Logger, opts ...SupervisorOption) *Supervisor {
	s := &Supervisor{
		client:       client,
		launcher:     launcher,
		log:          log,
		interval:     DefaultProbeInterval,
		readyTimeout: DefaultReadyTimeout,
		kick:         make(chan struct{}, 1),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Kick asks the supervisor to probe (and if needed relaunch) immediately
// instead of waiting out the probe interval. Non-blocking; used by the
// device backend when an action hits a dead WDA.
func (s *Supervisor) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run supervises until ctx is cancelled, then stops any host-side runner
// process. Call it on its own goroutine.
func (s *Supervisor) Run(ctx context.Context) {
	defer s.launcher.Stop()
	backoff := s.interval
	for {
		if s.healthy(ctx) {
			backoff = s.interval
		} else if s.relaunch(ctx) {
			backoff = s.interval
		} else {
			// Failed launch or WDA never came up: back off so a phone
			// that is simply unplugged isn't hammered with launches.
			if backoff *= 2; backoff > maxLaunchBackoff {
				backoff = maxLaunchBackoff
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
		case <-time.After(backoff):
		}
	}
}

// healthy probes GET /status once.
func (s *Supervisor) healthy(ctx context.Context) bool {
	pctx, cancel := context.WithTimeout(ctx, defaultProbeTimeout)
	defer cancel()
	return s.client.Status(pctx) == nil
}

// relaunch launches the runner and waits for /status to come up within
// the readiness budget, reporting whether WDA became healthy.
func (s *Supervisor) relaunch(ctx context.Context) bool {
	s.log.Info("wda down; launching runner", "wda", s.client.BaseURL(), "launcher", s.launcher.String())
	if err := s.launcher.Launch(ctx); err != nil {
		s.log.Warn("wda launch failed", "wda", s.client.BaseURL(), "err", err)
		return false
	}
	deadline := time.Now().Add(s.readyTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
		if s.healthy(ctx) {
			s.log.Info("wda ready", "wda", s.client.BaseURL())
			return true
		}
	}
	s.log.Warn("wda did not become ready within budget", "wda", s.client.BaseURL(), "budget", s.readyTimeout)
	return false
}
