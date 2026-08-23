package registry

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/internal/actions/mirror"
	"github.com/BariBariGood/manzanas/proto"
)

// Labels carried by mirror-backed device targets.
const (
	// MirrorLabel marks a target driven through macOS iPhone Mirroring
	// (the mirrord helper).
	MirrorLabel = "mirror"
	// MirrorBlockedLabel marks a mirror target whose mirroring session
	// shows an interstitial (connect / "iPhone in Use" / paused): the
	// target stays enumerated and leasable, but input fails until the
	// interstitial clears.
	MirrorBlockedLabel = "mirror-blocked"
	// NoTunnelLabel marks a mirror target whose CoreDevice tunnel is
	// down: mirror input/observe work, but devicectl-routed actions
	// (install_app, launch_app, terminate_app) fail until the device
	// is reachable over USB or Wi-Fi.
	NoTunnelLabel = "no-tunnel"
	// NoDevicectlLabel marks a mirror target that devicectl does not
	// enumerate at all (unpaired host): the target is synthesized from
	// the mirror config, and every devicectl-routed action fails.
	NoDevicectlLabel = "no-devicectl"
)

// mirrorStatusTTL bounds how often the registry probes mirrord: /v1/status
// runs a capture + OCR on mirrord's single serial worker, so registry
// List/Get polling must not contend with live gestures.
const mirrorStatusTTL = 5 * time.Second

// mirrorProbeTimeout bounds one status probe so a wedged helper cannot
// stall target enumeration for the client's full 60s timeout.
const mirrorProbeTimeout = 10 * time.Second

// MirrorRegistry enumerates mirror-backed devices from the configured
// mirror UDID set, deriving liveness from the mirrord helper's status.
// It exists so a mirror-only host (no CoreDevice pairing, so devicectl
// enumerates nothing) still surfaces its phone in /v0/targets; when
// devicectl does know the UDID, MergedRegistry overlays this registry's
// liveness onto devicectl's metadata (see merge.go).
type MirrorRegistry struct {
	probe func(ctx context.Context, socket string) (mirror.Status, error)
	now   func() time.Time
	ttl   time.Duration

	mu      sync.Mutex
	mirrors map[string]string // udid -> mirrord socket
	cache   map[string]mirrorCacheEntry
}

type mirrorCacheEntry struct {
	socket  string
	status  mirror.Status
	err     error
	expires time.Time
}

// NewMirror returns a MirrorRegistry. probe fetches a mirrord status for
// a socket path; nil selects the default probe (a mirror.Client per
// socket). The registry is empty until SetMirrors publishes a config.
func NewMirror(probe func(ctx context.Context, socket string) (mirror.Status, error)) *MirrorRegistry {
	if probe == nil {
		probe = defaultMirrorProbe()
	}
	return &MirrorRegistry{
		probe:   probe,
		now:     time.Now,
		ttl:     mirrorStatusTTL,
		mirrors: map[string]string{},
		cache:   map[string]mirrorCacheEntry{},
	}
}

// mirrorFullStatusInterval is how often the default probe pays for a
// full (capture+OCR) status to keep blocked-interstitial detection: light
// probes in between keep the helper's serial worker free for gestures.
const mirrorFullStatusInterval = time.Minute

// defaultMirrorProbe keeps one mirror.Client per socket so repeated
// probes reuse the client (and its serializing mutex). It mostly probes
// the light status (no capture+OCR on the helper's serial worker, so a
// liveness poll can never stall live gestures behind an OCR pass), but
// light probes cannot see a blocked interstitial, so a full status runs
// every mirrorFullStatusInterval — and keeps running while the session
// is blocked, so the "mirror-blocked" label holds steady until the
// interstitial clears rather than flickering with the cache TTL.
// A helper predating the light mode gets the full status; the light
// route is re-tried once per interval so an in-place helper upgrade is
// picked up without a daemon restart.
func defaultMirrorProbe() func(ctx context.Context, socket string) (mirror.Status, error) {
	return newDefaultMirrorProbe(time.Now)
}

func newDefaultMirrorProbe(now func() time.Time) func(ctx context.Context, socket string) (mirror.Status, error) {
	type entry struct {
		client       *mirror.Client
		blocked      bool
		lastFull     time.Time
		retryLightAt time.Time
	}
	var mu sync.Mutex
	clients := map[string]*entry{}
	return func(ctx context.Context, socket string) (mirror.Status, error) {
		mu.Lock()
		e, ok := clients[socket]
		if !ok {
			e = &entry{client: mirror.New(socket)}
			clients[socket] = e
		}
		t := now()
		light := !e.blocked &&
			t.Sub(e.lastFull) < mirrorFullStatusInterval &&
			!t.Before(e.retryLightAt)
		mu.Unlock()

		if light {
			st, err := e.client.StatusLight(ctx)
			if err == nil || !mirror.IsUnsupportedRoute(err) {
				return st, err
			}
			// Old helper: fall through to the full status, and only
			// re-try the light route after the interval so an upgraded
			// helper is picked up without paying a 404 on every poll.
			mu.Lock()
			e.retryLightAt = now().Add(mirrorFullStatusInterval)
			mu.Unlock()
		}
		st, err := e.client.Status(ctx)
		// Record the attempt even on failure: a full probe that timed out
		// (likely because the worker was busy with a gesture) must not
		// force the expensive path again on every following poll.
		mu.Lock()
		e.lastFull = now()
		e.blocked = err == nil && st.State == "blocked"
		mu.Unlock()
		return st, err
	}
}

// SupplementalRegistry marks this registry as supplemental for
// MergedRegistry's error accounting: List never fails (probe errors are
// liveness, not enumeration failures), so its success must not hide a
// total outage of the fallible registries it is merged with.
func (r *MirrorRegistry) SupplementalRegistry() {}

// SetMirrors publishes the effective mirror config (udid -> mirrord
// socket), replacing the previous set. Called by the devices manager on
// every config apply. Cache entries for removed or re-socketed UDIDs are
// dropped so a config change takes effect immediately.
func (r *MirrorRegistry) SetMirrors(mirrors map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]string, len(mirrors))
	for u, s := range mirrors {
		next[u] = s
	}
	for u, e := range r.cache {
		if s, ok := next[u]; !ok || s != e.socket {
			delete(r.cache, u)
		}
	}
	r.mirrors = next
}

// statusFor returns the (possibly cached) mirrord status for a UDID. The
// lock is held across the probe: mirrord is a single serial worker, so
// concurrent duplicate probes would only pile onto its queue.
func (r *MirrorRegistry) statusFor(ctx context.Context, udid string) (mirror.Status, error, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	socket, ok := r.mirrors[udid]
	if !ok {
		return mirror.Status{}, nil, false
	}
	if e, ok := r.cache[udid]; ok && e.socket == socket && r.now().Before(e.expires) {
		return e.status, e.err, true
	}
	pctx, cancel := context.WithTimeout(ctx, mirrorProbeTimeout)
	st, err := r.probe(pctx, socket)
	cancel()
	if err != nil && ctx.Err() != nil {
		// The caller went away mid-probe: that says nothing about the
		// helper, so do not poison the cache with it.
		return st, err, true
	}
	r.cache[udid] = mirrorCacheEntry{socket: socket, status: st, err: err, expires: r.now().Add(r.ttl)}
	return st, err, true
}

// mirrorTarget synthesizes a target for a mirror UDID from its mirrord
// status. The NoDevicectlLabel is always attached here; the merge overlay
// strips it when devicectl does enumerate the UDID.
func mirrorTarget(udid string, st mirror.Status, err error) proto.Target {
	labels := []string{string(proto.TargetDevice), MirrorLabel, NoDevicectlLabel}
	state := proto.StateUnknown
	switch {
	case err != nil:
		labels = append(labels, DisconnectedLabel)
	case st.State == "ready":
		state = proto.StateBooted
	case st.State == "blocked":
		// Enumerated and leasable: the phone is mirrored but an
		// interstitial blocks input. Booted + label is friendlier to
		// lease selectors than Unknown.
		state = proto.StateBooted
		labels = append(labels, MirrorBlockedLabel)
	default: // "not-running", "no-window", anything unrecognized
		labels = append(labels, DisconnectedLabel)
	}
	return proto.Target{
		UDID:   udid,
		Kind:   proto.TargetDevice,
		Name:   "iPhone (mirror)",
		State:  state,
		Labels: labels,
	}
}

func (r *MirrorRegistry) List(ctx context.Context) ([]proto.Target, error) {
	r.mu.Lock()
	udids := make([]string, 0, len(r.mirrors))
	for u := range r.mirrors {
		udids = append(udids, u)
	}
	r.mu.Unlock()
	sort.Strings(udids)
	var out []proto.Target
	for _, u := range udids {
		st, err, ok := r.statusFor(ctx, u)
		if !ok {
			continue // removed by a concurrent SetMirrors
		}
		out = append(out, mirrorTarget(u, st, err))
	}
	return out, nil
}

func (r *MirrorRegistry) Get(ctx context.Context, udid string) (proto.Target, error) {
	st, err, ok := r.statusFor(ctx, udid)
	if !ok {
		return proto.Target{}, &NotFoundError{UDID: udid}
	}
	return mirrorTarget(udid, st, err), nil
}

// Boot and Shutdown mirror DevicectlRegistry's refusals: a physical phone
// cannot be booted or powered off through the daemon.
func (r *MirrorRegistry) Boot(ctx context.Context, udid string) error {
	if _, err := r.Get(ctx, udid); err != nil {
		return err
	}
	return &DeviceBootError{UDID: udid}
}

func (r *MirrorRegistry) Shutdown(ctx context.Context, udid string) error {
	if _, err := r.Get(ctx, udid); err != nil {
		return err
	}
	return &DeviceShutdownError{UDID: udid}
}

func (r *MirrorRegistry) Health(ctx context.Context, udid string) (proto.TargetState, error) {
	t, err := r.Get(ctx, udid)
	if err != nil {
		return proto.StateUnknown, err
	}
	return t.State, nil
}
