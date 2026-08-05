// Package lease implements the in-memory lease manager: TTL-bounded
// exclusive claims on targets, label-based matching, and a FIFO queue per
// label-set.
package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

const (
	DefaultTTL = 300 * time.Second
	MaxTTL     = 3600 * time.Second
	// TerminalRetention is how long released/expired leases stay readable
	// via Get before being garbage-collected.
	TerminalRetention = 10 * time.Minute
	// QueueWaitTTL is how long a queued lease may sit in the queue without
	// its owner polling Get before it expires, so abandoned requests don't
	// block the queue or claim freed targets. Each Get refreshes it.
	QueueWaitTTL = 30 * time.Minute
)

var (
	// ErrNoMatch means no target (leased or free) matches the labels.
	ErrNoMatch = errors.New("no target matches requested labels")
	// ErrNotFound means the lease ID is unknown.
	ErrNotFound = errors.New("lease not found")
	// ErrNotActive means the lease is queued/expired/released.
	ErrNotActive = errors.New("lease is not active")
	// ErrResetInFlight means the target's post-lease reset is still
	// running, so its quarantine cannot be cleared yet.
	ErrResetInFlight = errors.New("reset is still in flight")
)

// GrantFunc is invoked (outside the manager lock) when a queued lease
// becomes active or an active lease expires.
type GrantFunc func(l proto.Lease)

// ResetFunc performs an ended lease's requested auto-reset (Lease.Reset:
// "erase" or "snapshot:<name>") on its target. It runs on its own
// goroutine; the manager keeps the target held until it returns nil, then
// frees it for the next holder. If it returns an error the target stays
// quarantined (never handed out dirty) until FinishReset is called.
type ResetFunc func(l proto.Lease) error

// resetSentinel occupies byTarget while a post-lease reset is in flight so
// the target can't be acquired or promoted mid-reset.
const resetSentinel = "__resetting__"

// reserveSentinel occupies byTarget while pool lifecycle work (recycle,
// adoption, re-provision) runs so the target can't be granted mid-wipe.
const reserveSentinel = "__reserved__"

// Manager is a concurrency-safe in-memory lease table.
type Manager struct {
	reg     registry.Registry
	now     func() time.Time
	onEvent GrantFunc
	// onActive fires (outside the lock) every time a lease becomes
	// active — immediate grants included, which never reach onEvent.
	// It must be fast and must not call back into the manager.
	onActive GrantFunc
	resetFn  ResetFunc

	mu     sync.Mutex
	leases map[string]*proto.Lease
	// targets caches target metadata by UDID (refreshed on Acquire) so
	// queue promotion never calls the registry under the lock.
	targets map[string]proto.Target
	// byTarget maps target UDID -> active lease ID.
	byTarget map[string]string
	// queues maps canonical label-set key -> FIFO of queued lease IDs.
	queues map[string][]string
	// terminalAt records when a lease entered a terminal state, for GC.
	terminalAt map[string]time.Time
	// queueDeadline maps queued lease ID -> abandonment deadline,
	// refreshed whenever the owner polls Get.
	queueDeadline map[string]time.Time
	// pendingResets holds leases whose expiry requested an auto-reset;
	// drained by takeResetsLocked at each unlock point.
	pendingResets []proto.Lease
	// resetInFlight marks targets whose reset goroutine is still running,
	// distinguishing them from targets quarantined by a failed reset.
	resetInFlight map[string]bool

	stop chan struct{}
	once sync.Once
}

// New creates a Manager over the given registry. onEvent may be nil.
func New(reg registry.Registry, onEvent GrantFunc) *Manager {
	m := newManager(reg, onEvent)
	go m.expiryLoop()
	return m
}

// newManager builds a Manager without starting the background expiry loop,
// so tests can swap the clock/callback before any goroutine reads them.
func newManager(reg registry.Registry, onEvent GrantFunc) *Manager {
	return &Manager{
		reg:           reg,
		now:           func() time.Time { return time.Now().UTC() }, // PROTOCOL.md: timestamps are RFC 3339 UTC
		onEvent:       onEvent,
		leases:        make(map[string]*proto.Lease),
		targets:       make(map[string]proto.Target),
		byTarget:      make(map[string]string),
		queues:        make(map[string][]string),
		terminalAt:    make(map[string]time.Time),
		queueDeadline: make(map[string]time.Time),
		resetInFlight: make(map[string]bool),
		stop:          make(chan struct{}),
	}
}

// SetOnActive wires a hook invoked whenever a lease becomes active,
// including immediate grants (which produce no onEvent event). The warm
// pool uses it to thaw parked simulators the moment they are handed out.
// Must be set before the manager serves traffic.
func (m *Manager) SetOnActive(fn GrantFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onActive = fn
}

// SetResetFunc wires the post-lease auto-reset hook (owned by the state
// slice). Must be set before the manager serves traffic.
func (m *Manager) SetResetFunc(fn ResetFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resetFn = fn
}

// DropReset clears a lease's reset spec so a subsequent Release skips the
// post-lease reset machinery entirely. Used when a grant is refused
// before the holder ever touched the target: nothing to clean, and the
// target must not be sentineled for a reset that will never run.
func (m *Manager) DropReset(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.leases[id]; ok {
		l.Reset = ""
	}
}

// needsResetLocked reports whether an ended active lease requested a reset
// that the manager can run.
func (m *Manager) needsResetLocked(l *proto.Lease) bool {
	return m.resetFn != nil && l.Reset != "" && l.Reset != "none" && l.TargetUDID != ""
}

// takeResetsLocked drains resets queued by expireLocked.
func (m *Manager) takeResetsLocked() []proto.Lease {
	resets := m.pendingResets
	m.pendingResets = nil
	return resets
}

// startResets runs each ended lease's reset on its own goroutine, freeing
// the target when it succeeds. A failed reset leaves the sentinel in
// place: the target is quarantined rather than handed out dirty.
func (m *Manager) startResets(resets []proto.Lease) {
	for _, l := range resets {
		go func(l proto.Lease) {
			err := m.resetFn(l)
			m.mu.Lock()
			delete(m.resetInFlight, l.TargetUDID)
			m.mu.Unlock()
			if err != nil {
				return
			}
			m.FinishReset(l.TargetUDID)
		}(l)
	}
}

// ClearQuarantine is the operator path for freeing a target quarantined by
// a failed reset. Unlike FinishReset it refuses while the reset is still
// running, so a queued lease is never promoted onto a sim mid-wipe. It
// reports whether a quarantine was actually cleared (false = no-op on a
// target that wasn't quarantined).
func (m *Manager) ClearQuarantine(udid string) (bool, error) {
	m.mu.Lock()
	if m.resetInFlight[udid] {
		m.mu.Unlock()
		return false, ErrResetInFlight
	}
	m.mu.Unlock()
	return m.FinishReset(udid), nil
}

// FinishReset frees a target held for a post-lease reset and promotes the
// next queued lease matching it, reporting whether it did so. It is a
// no-op (false) unless the target is actually held by the reset sentinel,
// so a stray call can never double-assign a busy target.
func (m *Manager) FinishReset(udid string) bool {
	m.mu.Lock()
	if m.byTarget[udid] != resetSentinel {
		m.mu.Unlock()
		return false
	}
	delete(m.byTarget, udid)
	granted := m.promoteLocked(udid)
	m.mu.Unlock()
	m.emit(granted)
	return true
}

// Reserve holds a free target for pool lifecycle work (shutdown, erase,
// slim) so it cannot be granted mid-wipe. It fails (ok=false) if the
// target is already leased, mid-reset, or reserved; the caller must skip
// the target in that case. A target quarantined by a FAILED reset may be
// taken over (takeover=true): pool recovery is the automatic path out of
// quarantine, and the caller MUST erase the target before releasing it
// via Unreserve — or Quarantine again on failure — because it may still
// carry the previous holder's data. Pair with Unreserve or Quarantine.
func (m *Manager) Reserve(udid string) (takeover, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, held := m.byTarget[udid]; held {
		if id != resetSentinel || m.resetInFlight[udid] {
			return false, false
		}
		takeover = true
	}
	m.byTarget[udid] = reserveSentinel
	return takeover, true
}

// Quarantine converts a Reserve hold back into a failed-reset
// quarantine: the pool's rebuild of the target failed, so it must not
// be granted until a later rebuild (or ClearQuarantine) succeeds.
// No-op unless the reserve sentinel holds the target.
func (m *Manager) Quarantine(udid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byTarget[udid] == reserveSentinel {
		m.byTarget[udid] = resetSentinel
	}
}

// Unreserve frees a Reserve hold and promotes the next queued lease
// matching the target. No-op unless the reserve sentinel holds it.
func (m *Manager) Unreserve(udid string) {
	m.mu.Lock()
	if m.byTarget[udid] != reserveSentinel {
		m.mu.Unlock()
		return
	}
	delete(m.byTarget, udid)
	granted := m.promoteLocked(udid)
	m.mu.Unlock()
	m.emit(granted)
}

// Close stops the background expiry loop.
func (m *Manager) Close() { m.once.Do(func() { close(m.stop) }) }

func (m *Manager) expiryLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.ExpireNow()
		}
	}
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "lse_" + hex.EncodeToString(b)
}

// queueKey canonicalizes a request's matching criteria (label-set plus
// any pinned UDID) so FIFO order holds per distinct criteria.
func queueKey(labels []string, udid string) string {
	c := append([]string(nil), labels...)
	sort.Strings(c)
	key := strings.Join(c, ",")
	if udid != "" {
		key += "|udid=" + udid
	}
	return key
}

func (m *Manager) leaseQueueKey(l *proto.Lease) string {
	return queueKey(l.Labels, l.RequestedUDID)
}

func matches(target proto.Target, labels []string) bool {
	set := make(map[string]bool, len(target.Labels))
	for _, l := range target.Labels {
		set[l] = true
	}
	for _, l := range labels {
		if !set[l] {
			return false
		}
	}
	return true
}

// matchesRequest applies both the label-set and any pinned UDID.
func matchesRequest(target proto.Target, labels []string, udid string) bool {
	if udid != "" && target.UDID != udid {
		return false
	}
	return matches(target, labels)
}

func clampTTL(seconds int) time.Duration {
	if seconds <= 0 {
		return DefaultTTL
	}
	if seconds > int(MaxTTL/time.Second) {
		return MaxTTL
	}
	return time.Duration(seconds) * time.Second
}

// Acquire grants a lease on a free target matching all req.Labels, or queues
// the request FIFO if all matching targets are leased. Returns ErrNoMatch if
// no target matches at all.
func (m *Manager) Acquire(ctx context.Context, req proto.AcquireLeaseRequest) (proto.Lease, error) {
	targets, err := m.reg.List(ctx)
	if err != nil {
		return proto.Lease{}, err
	}

	m.mu.Lock()
	// Rebuild the cache from the fresh listing before expiring, so vanished
	// simulators are pruned and never handed to queued leases by promoteLocked.
	m.targets = make(map[string]proto.Target, len(targets))
	for _, t := range targets {
		m.targets[t.UDID] = t
	}
	pending := m.expireLocked()
	// Offer any free targets (e.g. simulators that appeared since the last
	// refresh) to already-queued leases before considering this request,
	// preserving FIFO per label-set.
	for _, t := range targets {
		if _, leased := m.byTarget[t.UDID]; !leased {
			pending = append(pending, m.promoteLocked(t.UDID)...)
		}
	}
	// grantedNow carries an immediately-granted lease out of the lock so
	// onActive fires after unlock (queued grants reach onActive via emit).
	var grantedNow *proto.Lease
	defer func() {
		resets := m.takeResetsLocked()
		onActive := m.onActive
		m.mu.Unlock()
		if grantedNow != nil && onActive != nil {
			onActive(*grantedNow)
		}
		m.emit(pending)
		m.startResets(resets)
	}()

	var free *proto.Target
	anyMatch := false
	for i := range targets {
		if !matchesRequest(targets[i], req.Labels, req.UDID) {
			continue
		}
		// A reset-carrying request can never be granted a physical
		// device (mirrors promoteLocked), so a device does not count as
		// a match either: when only devices match, the caller gets a
		// clean no_match instead of an un-grantable queue slot.
		if req.Reset != "" && req.Reset != "none" && targets[i].Kind == proto.TargetDevice {
			continue
		}
		anyMatch = true
		if _, leased := m.byTarget[targets[i].UDID]; !leased && free == nil {
			free = &targets[i]
		}
	}
	if !anyMatch {
		return proto.Lease{}, ErrNoMatch
	}
	// Don't jump ahead of earlier requests still queued for this label-set.
	if len(m.queues[queueKey(req.Labels, req.UDID)]) > 0 {
		free = nil
	}

	ttl := clampTTL(req.TTLSeconds)
	l := &proto.Lease{
		ID:            newID(),
		Labels:        append([]string(nil), req.Labels...),
		AgentID:       req.AgentID,
		Purpose:       req.Purpose,
		TTLSeconds:    int(ttl.Seconds()),
		CreatedAt:     m.now(),
		Reset:         req.Reset,
		Record:        req.Record,
		RequestedUDID: req.UDID,
	}
	if free != nil {
		l.State = proto.LeaseActive
		l.TargetUDID = free.UDID
		exp := m.now().Add(ttl)
		l.ExpiresAt = &exp
		m.byTarget[free.UDID] = l.ID
		snap := *l
		grantedNow = &snap
	} else {
		l.State = proto.LeaseQueued
		key := queueKey(req.Labels, req.UDID)
		m.queues[key] = append(m.queues[key], l.ID)
		l.QueuePosition = len(m.queues[key])
		m.queueDeadline[l.ID] = m.now().Add(QueueWaitTTL)
	}
	m.leases[l.ID] = l
	return *l, nil
}

// Get returns a lease by ID (with a fresh queue position for queued leases).
func (m *Manager) Get(id string) (proto.Lease, error) {
	m.mu.Lock()
	pending := m.expireLocked()
	defer func() {
		resets := m.takeResetsLocked()
		m.mu.Unlock()
		m.emit(pending)
		m.startResets(resets)
	}()
	l, ok := m.leases[id]
	if !ok {
		return proto.Lease{}, ErrNotFound
	}
	if l.State == proto.LeaseQueued {
		l.QueuePosition = m.queuePositionLocked(l)
		m.queueDeadline[l.ID] = m.now().Add(QueueWaitTTL)
	}
	return *l, nil
}

// Peek returns a lease by ID without running expiry or emitting events,
// so it is safe to call from event callbacks (Get is not: it can
// synchronously re-enter the caller via emit).
func (m *Manager) Peek(id string) (proto.Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[id]
	if !ok {
		return proto.Lease{}, ErrNotFound
	}
	return *l, nil
}

func (m *Manager) queuePositionLocked(l *proto.Lease) int {
	for i, id := range m.queues[m.leaseQueueKey(l)] {
		if id == l.ID {
			return i + 1
		}
	}
	return 0
}

// Renew extends an active lease's TTL.
func (m *Manager) Renew(id string, ttlSeconds int) (proto.Lease, error) {
	m.mu.Lock()
	pending := m.expireLocked()
	defer func() {
		resets := m.takeResetsLocked()
		m.mu.Unlock()
		m.emit(pending)
		m.startResets(resets)
	}()
	l, ok := m.leases[id]
	if !ok {
		return proto.Lease{}, ErrNotFound
	}
	if l.State != proto.LeaseActive {
		return *l, ErrNotActive
	}
	ttl := clampTTL(ttlSeconds)
	if ttlSeconds <= 0 {
		ttl = clampTTL(l.TTLSeconds)
	}
	l.TTLSeconds = int(ttl.Seconds())
	exp := m.now().Add(ttl)
	l.ExpiresAt = &exp
	return *l, nil
}

// Release ends a lease (active or queued) and promotes the next queued
// lease that matches the freed target.
func (m *Manager) Release(ctx context.Context, id string) (proto.Lease, error) {
	m.mu.Lock()
	granted := m.expireLocked()
	l, ok := m.leases[id]
	if !ok {
		resets := m.takeResetsLocked()
		m.mu.Unlock()
		m.emit(granted)
		m.startResets(resets)
		return proto.Lease{}, ErrNotFound
	}
	var resets []proto.Lease
	switch l.State {
	case proto.LeaseActive:
		delete(m.byTarget, l.TargetUDID)
		l.State = proto.LeaseReleased
		m.terminalAt[l.ID] = m.now()
		if m.needsResetLocked(l) {
			m.byTarget[l.TargetUDID] = resetSentinel
			m.resetInFlight[l.TargetUDID] = true
			resets = append(resets, *l)
		} else {
			granted = append(granted, m.promoteLocked(l.TargetUDID)...)
		}
	case proto.LeaseQueued:
		m.removeFromQueueLocked(l)
		delete(m.queueDeadline, l.ID)
		l.State = proto.LeaseReleased
		m.terminalAt[l.ID] = m.now()
	}
	out := *l
	resets = append(resets, m.takeResetsLocked()...)
	m.mu.Unlock()
	m.emit(granted)
	m.startResets(resets)
	return out, nil
}

// Active returns the active lease for a target UDID, if any.
func (m *Manager) Active(udid string) (proto.Lease, bool) {
	m.mu.Lock()
	pending := m.expireLocked()
	defer func() {
		resets := m.takeResetsLocked()
		m.mu.Unlock()
		m.emit(pending)
		m.startResets(resets)
	}()
	id, ok := m.byTarget[udid]
	if !ok || id == resetSentinel || id == reserveSentinel {
		return proto.Lease{}, false
	}
	return *m.leases[id], true
}

// ActiveCount returns the number of currently active leases.
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, l := range m.leases {
		if l.State == proto.LeaseActive {
			n++
		}
	}
	return n
}

// QueuedCount returns the total number of queued leases across all FIFOs.
func (m *Manager) QueuedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, q := range m.queues {
		n += len(q)
	}
	return n
}

// ExpireNow expires overdue leases and promotes queued ones.
func (m *Manager) ExpireNow() {
	m.mu.Lock()
	granted := m.expireLocked()
	resets := m.takeResetsLocked()
	m.mu.Unlock()
	m.emit(granted)
	m.startResets(resets)
}

func (m *Manager) expireLocked() []proto.Lease {
	var events []proto.Lease
	now := m.now()
	// Drop abandoned queued leases first so they don't block the queue
	// or claim targets freed by the expiry pass below.
	for id, deadline := range m.queueDeadline {
		if !now.After(deadline) {
			continue
		}
		l := m.leases[id]
		m.removeFromQueueLocked(l)
		delete(m.queueDeadline, id)
		l.State = proto.LeaseExpired
		m.terminalAt[id] = now
		events = append(events, *l)
	}
	for _, l := range m.leases {
		if l.State == proto.LeaseActive && l.ExpiresAt != nil && now.After(*l.ExpiresAt) {
			delete(m.byTarget, l.TargetUDID)
			l.State = proto.LeaseExpired
			m.terminalAt[l.ID] = now
			events = append(events, *l)
			if m.needsResetLocked(l) {
				m.byTarget[l.TargetUDID] = resetSentinel
				m.resetInFlight[l.TargetUDID] = true
				m.pendingResets = append(m.pendingResets, *l)
			} else {
				events = append(events, m.promoteLocked(l.TargetUDID)...)
			}
		}
	}
	for id, at := range m.terminalAt {
		if now.Sub(at) > TerminalRetention {
			delete(m.terminalAt, id)
			delete(m.leases, id)
		}
	}
	return events
}

// promoteLocked hands a freed target to the first queued lease whose labels
// match it, using the cached target metadata (never the registry, which may
// shell out). Returns granted leases for event emission.
func (m *Manager) promoteLocked(udid string) []proto.Lease {
	target, ok := m.targets[udid]
	if !ok {
		return nil
	}
	for key, q := range m.queues {
		for i, id := range q {
			l := m.leases[id]
			if !matchesRequest(target, l.Labels, l.RequestedUDID) {
				continue
			}
			// A reset-carrying lease must never land on a physical
			// device (there is no erase/snapshot for devices); it keeps
			// waiting for a resettable target instead.
			if l.Reset != "" && l.Reset != "none" && target.Kind == proto.TargetDevice {
				continue
			}
			m.queues[key] = append(q[:i], q[i+1:]...)
			if len(m.queues[key]) == 0 {
				delete(m.queues, key)
			}
			delete(m.queueDeadline, id)
			ttl := clampTTL(l.TTLSeconds)
			l.State = proto.LeaseActive
			l.TargetUDID = udid
			l.QueuePosition = 0
			exp := m.now().Add(ttl)
			l.ExpiresAt = &exp
			m.byTarget[udid] = l.ID
			return []proto.Lease{*l}
		}
	}
	return nil
}

func (m *Manager) removeFromQueueLocked(l *proto.Lease) {
	key := m.leaseQueueKey(l)
	q := m.queues[key]
	for i, id := range q {
		if id == l.ID {
			m.queues[key] = append(q[:i], q[i+1:]...)
			break
		}
	}
	if len(m.queues[key]) == 0 {
		delete(m.queues, key)
	}
}

func (m *Manager) emit(events []proto.Lease) {
	m.mu.Lock()
	onActive, onEvent := m.onActive, m.onEvent
	m.mu.Unlock()
	for _, e := range events {
		if e.State == proto.LeaseActive && onActive != nil {
			onActive(e)
		}
		if onEvent != nil {
			onEvent(e)
		}
	}
}
