package actions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"
)

// PoolConfig tunes the warm helper pool.
type PoolConfig struct {
	// MaxTargets caps how many simulators keep a warm helper at once.
	MaxTargets int
	// IdleTTL shuts a helper down after this much inactivity.
	IdleTTL time.Duration
	// OpTimeout bounds a single helper call; a helper that blows it is
	// treated as wedged (transport failure: evicted, caller falls back
	// cold) so one stuck process cannot stall its simulator forever.
	OpTimeout time.Duration
	// SpawnCooldown skips warm attempts for a UDID after a helper failed
	// to start, so actions go straight cold instead of paying the failed
	// bootstrap on every call.
	SpawnCooldown time.Duration
	// Logger receives lifecycle events; nil discards them.
	Logger *slog.Logger
}

const (
	// DefaultMaxWarmTargets caps concurrently warm simulators.
	DefaultMaxWarmTargets = 4
	// DefaultWarmIdleTTL is how long an unused helper stays resident.
	DefaultWarmIdleTTL = 5 * time.Minute
	// DefaultWarmOpTimeout bounds one helper call (describe-ui on a large
	// tree takes a few seconds; anything past this means a wedged helper).
	DefaultWarmOpTimeout = 30 * time.Second
	// DefaultSpawnCooldown is how long a UDID stays cold after its helper
	// failed to start.
	DefaultSpawnCooldown = 30 * time.Second
)

// Pool keeps at most MaxTargets warm helpers, one per UDID: it spins one
// up on first use, restarts it once when a call hits a transport failure,
// and shuts it down after IdleTTL of inactivity.
type Pool struct {
	factory HelperFactory
	cfg     PoolConfig
	clock   func() time.Time

	mu        sync.Mutex
	entries   map[string]*poolEntry
	failUntil map[string]time.Time
	closed    bool
	stop      chan struct{}
	janitor   sync.Once
}

// poolEntry serializes access to one helper: helpers handle one op at a
// time, and restarts must not race with in-flight calls.
type poolEntry struct {
	mu       sync.Mutex
	helper   Helper
	lastUsed time.Time
	// gone marks an entry removed from the pool's map (evicted, reaped,
	// or closed); a caller that raced the removal must re-fetch a slot
	// instead of spawning an untracked helper on the detached entry.
	gone bool
}

// NewPool builds a warm pool over the given factory. Zero config fields
// take the defaults above.
func NewPool(factory HelperFactory, cfg PoolConfig) *Pool {
	if cfg.MaxTargets <= 0 {
		cfg.MaxTargets = DefaultMaxWarmTargets
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = DefaultWarmIdleTTL
	}
	if cfg.OpTimeout <= 0 {
		cfg.OpTimeout = DefaultWarmOpTimeout
	}
	if cfg.SpawnCooldown <= 0 {
		cfg.SpawnCooldown = DefaultSpawnCooldown
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Pool{
		factory:   factory,
		cfg:       cfg,
		clock:     time.Now,
		entries:   map[string]*poolEntry{},
		failUntil: map[string]time.Time{},
		stop:      make(chan struct{}),
	}
}

// Call runs one op on the UDID's warm helper, starting or restarting it as
// needed. A transport failure evicts the helper and retries once on a
// fresh one; a second failure is returned to the caller (who falls back to
// the cold path).
func (p *Pool) Call(ctx context.Context, udid, op string, args map[string]any) (map[string]any, error) {
	for attempt := 0; attempt < 4; attempt++ {
		e, err := p.entry(udid)
		if err != nil {
			return nil, err
		}
		e.mu.Lock()
		if e.gone {
			// Lost a race with eviction/close between entry() and locking;
			// the entry is detached from the map, so grab a fresh slot.
			e.mu.Unlock()
			continue
		}
		return p.callLocked(ctx, udid, e, op, args)
	}
	return nil, transportErr("could not obtain a warm slot for %s", udid)
}

// callLocked runs the op on e's helper; e.mu is held by the caller.
func (p *Pool) callLocked(ctx context.Context, udid string, e *poolEntry, op string, args map[string]any) (map[string]any, error) {
	defer e.mu.Unlock()
	var err error
	if e.helper == nil {
		if e.helper, err = p.factory(udid); err != nil {
			p.noteSpawnFailure(udid)
			p.evict(udid, e)
			return nil, err
		}
	}
	opCtx, cancel := context.WithTimeout(ctx, p.cfg.OpTimeout)
	defer cancel()
	res, err := e.helper.Call(opCtx, op, args)
	// Replay on a fresh helper only when the request never reached the
	// old one: a Delivered failure may already have moved the simulator
	// (a tap that landed), so it must surface to the caller instead.
	if te, isTransport := asTransport(err); isTransport && !te.Delivered && opCtx.Err() == nil {
		p.cfg.Logger.Warn("warm helper failed; restarting", "udid", udid, "err", err)
		_ = e.helper.Close()
		if e.helper, err = p.factory(udid); err != nil {
			p.noteSpawnFailure(udid)
			p.evict(udid, e)
			return nil, err
		}
		res, err = e.helper.Call(opCtx, op, args)
	}
	if _, isTransport := asTransport(err); isTransport {
		_ = e.helper.Close()
		// A caller-cancelled request says nothing about helper health, so
		// it must not put the target on spawn cooldown.
		if ctx.Err() == nil {
			p.noteSpawnFailure(udid)
		}
		p.evict(udid, e)
		return nil, err
	}
	e.lastUsed = p.clock()
	return res, err
}

// entry returns the pool slot for a UDID, creating it (and evicting the
// least-recently-used idle slot when at capacity) as needed. Any evicted
// helper is closed after the pool lock is released, so a slow shutdown
// cannot stall other simulators.
func (p *Pool) entry(udid string) (*poolEntry, error) {
	e, evicted, err := p.entryLocked(udid)
	if evicted != nil {
		_ = evicted.Close()
	}
	return e, err
}

func (p *Pool) entryLocked(udid string) (*poolEntry, Helper, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, transportErr("warm pool is closed")
	}
	p.janitor.Do(func() { go p.janitorLoop() })
	if e, ok := p.entries[udid]; ok {
		return e, nil, nil
	}
	if until, ok := p.failUntil[udid]; ok {
		if p.clock().Before(until) {
			return nil, nil, transportErr("helper for %s recently failed to start; cooling down", udid)
		}
		delete(p.failUntil, udid)
	}
	var evicted Helper
	if len(p.entries) >= p.cfg.MaxTargets {
		var ok bool
		if evicted, ok = p.evictLRULocked(); !ok {
			return nil, nil, transportErr("all %d warm slots are busy", p.cfg.MaxTargets)
		}
	}
	e := &poolEntry{lastUsed: p.clock()}
	p.entries[udid] = e
	return e, evicted, nil
}

// evictLRULocked detaches the least-recently-used entry whose helper is
// not mid-call. It returns the detached helper (nil if the slot was
// empty) for the caller to close outside the pool lock, and whether a
// slot was freed.
func (p *Pool) evictLRULocked() (Helper, bool) {
	var victimKey string
	var victim *poolEntry
	for k, e := range p.entries {
		if !e.mu.TryLock() {
			continue // in-flight call; not evictable
		}
		if victim == nil || e.lastUsed.Before(victim.lastUsed) {
			if victim != nil {
				victim.mu.Unlock()
			}
			victimKey, victim = k, e
		} else {
			e.mu.Unlock()
		}
	}
	if victim == nil {
		return nil, false
	}
	h := victim.helper
	victim.helper = nil
	victim.gone = true
	victim.mu.Unlock()
	delete(p.entries, victimKey)
	return h, true
}

// Drop detaches and closes the helper for a UDID, if any. Use it when
// the simulator was reset/erased/rebooted so the next action gets a
// fresh helper instead of a connection to pre-reset device state.
func (p *Pool) Drop(udid string) {
	p.mu.Lock()
	e, ok := p.entries[udid]
	if ok {
		delete(p.entries, udid)
	}
	delete(p.failUntil, udid)
	p.mu.Unlock()
	if !ok {
		return
	}
	e.mu.Lock()
	h := e.helper
	e.helper = nil
	e.gone = true
	e.mu.Unlock()
	if h != nil {
		_ = h.Close()
	}
}

// noteSpawnFailure puts a UDID on spawn cooldown so subsequent actions go
// straight to the cold path instead of re-paying a failed bootstrap.
func (p *Pool) noteSpawnFailure(udid string) {
	p.mu.Lock()
	p.failUntil[udid] = p.clock().Add(p.cfg.SpawnCooldown)
	p.mu.Unlock()
}

// evict removes an entry whose lock the caller already holds.
func (p *Pool) evict(udid string, e *poolEntry) {
	e.helper = nil
	e.gone = true
	p.mu.Lock()
	if cur, ok := p.entries[udid]; ok && cur == e {
		delete(p.entries, udid)
	}
	p.mu.Unlock()
}

// janitorLoop closes helpers idle past the TTL.
func (p *Pool) janitorLoop() {
	interval := p.cfg.IdleTTL / 2
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.reapIdle()
		}
	}
}

func (p *Pool) reapIdle() {
	now := p.clock()
	var reaped []Helper
	p.mu.Lock()
	for k, e := range p.entries {
		if !e.mu.TryLock() {
			continue
		}
		if e.helper != nil && now.Sub(e.lastUsed) >= p.cfg.IdleTTL {
			p.cfg.Logger.Info("warm helper idle; shutting down", "udid", k)
			reaped = append(reaped, e.helper)
			e.helper = nil
			e.gone = true
			delete(p.entries, k)
		}
		e.mu.Unlock()
	}
	p.mu.Unlock()
	for _, h := range reaped {
		_ = h.Close()
	}
}

// Close shuts down every helper and stops the janitor.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stop)
	entries := p.entries
	p.entries = map[string]*poolEntry{}
	p.mu.Unlock()
	for _, e := range entries {
		e.mu.Lock()
		if e.helper != nil {
			_ = e.helper.Close()
			e.helper = nil
		}
		e.gone = true
		e.mu.Unlock()
	}
}

// WarmCount reports how many helpers are currently resident (tests,
// diagnostics).
func (p *Pool) WarmCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

func asTransport(err error) (*TransportError, bool) {
	var te *TransportError
	if errors.As(err, &te) {
		return te, true
	}
	return nil, false
}
