package actions

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Element resolver ladder (#197): one element spec (tap_element
// {label: X}) resolves through an ordered ladder of resolution sources
// instead of a single hard-wired observe. Rungs today: (1) the a11y
// tree ("ax": AXe on simulators, WDA /source on devices); (2) reserved
// for the template/element-map rung (registered by the element-map
// slice once its store exists — the ladder is rung-pluggable, backends
// just return more rungs); (3) OCR text match ("ocr") where an OCR
// source exists (mirror-backed devices; WDA-only devices have no OCR
// source, so that rung simply isn't registered). When every rung misses
// for the whole wait budget the failure escalates with a compact
// observation bundle (screenshot + last text boxes) instead of a bare
// timeout, so the calling agent has evidence to re-plan on.

// Per-rung attempt budgets. Each rung attempt is bounded so a dead
// source (a WDA runner whose snapshot hangs) cannot eat the whole wait
// budget before the next rung gets a turn.
const (
	axRungBudget    = 1500 * time.Millisecond
	wdaAXRungBudget = 5 * time.Second
	ocrRungBudget   = 3 * time.Second
)

// resolverAX is the rung name the circuit breaker guards.
const resolverAX = "ax"

// errAXSourceDown is an errNotYet flavor for a transiently-down AX
// source (a WDA tunnel blip, a runner mid-relaunch): the wait keeps
// polling so the blip can heal, but if the source never comes back
// within the step the breaker is charged one failure.
var errAXSourceDown = fmt.Errorf("ax source down: %w", errNotYet)

// errResolverConfig marks a rung failure where the source was never
// contacted (no WDA endpoint configured for the device): a setup
// problem, not a source failure — surfaced as-is and never charged to
// the circuit breaker, so wiring the agent up mid-lease heals it.
var errResolverConfig = errors.New("resolver source not configured")

// errRungCannotServe marks a rung that can never resolve this matcher
// (the template rung with no candidate names, or none of them stored):
// the ladder treats the rung as absent for this resolve, preserving the
// fail-fast semantics of the remaining source rungs.
var errRungCannotServe = errors.New("rung cannot serve this matcher")

// resolver is one rung of the element-resolution ladder: a single
// observe-and-match attempt against one resolution source.
type resolver interface {
	// name identifies the rung in results and journal entries
	// ("ax", "ocr", "template", ...).
	name() string
	// budget bounds one resolve attempt.
	budget() time.Duration
	// resolve runs one observation poll and matches m against it. It
	// returns the match and the observation it came from; errNoMatch /
	// errNotYet mean "observed (or source not ready), no match — try
	// the next rung"; any other error is a rung failure (counted by the
	// circuit breaker for the AX rung) or a hard matcher error
	// (ambiguous_match, bad_request) that stops the ladder.
	resolve(ctx context.Context, udid string, m elementMatcher, refresh bool) (*Node, observation, error)
}

// resolverLadder is a backend's registered ladder for one target:
// ordered rungs plus the AX circuit-breaker state that guards rung 1.
type resolverLadder struct {
	rungs []resolver
	// breaker, when non-nil, guards the "ax" rung: after
	// axBreakerThreshold fast failures for the same lease+bundle the
	// rung is skipped for the rest of the lease.
	breaker *axBreaker
	// bundle is the launched bundle id the breaker keys on ("" when no
	// launch has been observed on the target).
	bundle string
}

// resolverProvider is implemented by an elementDriver that registers a
// resolution ladder; findElement routes through the ladder when present
// and keeps the direct observeOnce poll otherwise.
type resolverProvider interface {
	elementResolvers(udid string) resolverLadder
}

// matchIn matches m against one observation, passing the observation
// through so the ladder can keep the last text boxes for escalation.
func matchIn(obs observation, m elementMatcher) (*Node, observation, error) {
	hit, err := m.resolve(obs.nodes, obs.viewport)
	if err != nil {
		return nil, obs, err
	}
	return hit, obs, nil
}

// leaseIDKey carries the dispatching lease's ID through the action
// context, scoping the AX circuit breaker to one lease.
type leaseIDKey struct{}

// WithLeaseID returns a context carrying the lease ID of the dispatch.
func WithLeaseID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, leaseIDKey{}, id)
}

func leaseIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(leaseIDKey{}).(string)
	return id
}

// evidenceSinkKey marks a dispatch whose caller can actually store
// escalation screenshots (a journal store is configured); without it
// the ladder skips the capture rather than paying for pixels nobody
// keeps.
type evidenceSinkKey struct{}

// WithEvidenceSink returns a context telling the resolver ladder that
// escalation screenshot evidence has somewhere to go.
func WithEvidenceSink(ctx context.Context) context.Context {
	return context.WithValue(ctx, evidenceSinkKey{}, true)
}

func hasEvidenceSink(ctx context.Context) bool {
	ok, _ := ctx.Value(evidenceSinkKey{}).(bool)
	return ok
}

// axBreakerThreshold is the consecutive fast-failure count that opens
// the AX circuit for a lease+bundle.
const axBreakerThreshold = 3

// axBreakerMaxKeys bounds the breaker maps; the state is per-lease so
// entries go stale when leases end, and a full reset merely re-arms the
// probe attempts.
const axBreakerMaxKeys = 1024

// axBreaker is the per-bundle-id, per-lease circuit breaker for the AX
// rung: WDA's accessibility snapshot dies on hostile apps (TikTok-class)
// and takes the runner down with it, so after a few fast rung-1 failures
// for a bundle the ladder stops re-poking it for the rest of the lease.
// Containment, not cure: the supervisor still relaunches the runner.
type axBreaker struct {
	mu    sync.Mutex
	fails map[string]int
	open  map[string]bool
}

func newAXBreaker() *axBreaker {
	return &axBreaker{fails: map[string]int{}, open: map[string]bool{}}
}

// breakerKey scopes the breaker state: the lease (a fresh lease re-arms
// the probe) plus the launched bundle on the target. Without a lease in
// ctx the target UDID stands in, keeping the containment per-device.
func breakerKey(ctx context.Context, udid, bundle string) string {
	scope := leaseIDFrom(ctx)
	if scope == "" {
		scope = udid
	}
	return scope + "\x00" + bundle
}

func (b *axBreaker) isOpen(key string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open[key]
}

// fail records one fast rung failure, reporting whether it opened the
// circuit.
func (b *axBreaker) fail(key string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.fails) > axBreakerMaxKeys || len(b.open) > axBreakerMaxKeys {
		b.fails = map[string]int{}
		b.open = map[string]bool{}
	}
	b.fails[key]++
	if b.fails[key] >= axBreakerThreshold && !b.open[key] {
		b.open[key] = true
		return true
	}
	return b.open[key]
}

// succeed resets the consecutive-failure count. An already-open circuit
// stays open for the rest of the lease.
func (b *axBreaker) succeed(key string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.fails, key)
}

// evidenceCapturer is optionally implemented by a driver that can grab
// a screen capture for the escalation bundle.
type evidenceCapturer interface {
	captureEvidence(ctx context.Context, udid string) ([]byte, error)
}

// EscalationEvidence extracts a resolver-ladder escalation's observation
// bundle and screenshot from an action error (nil when the error is not
// an escalation). The server journals both.
func EscalationEvidence(err error) (map[string]any, []byte) {
	var ae *Error
	if errors.As(err, &ae) && ae.escalation != nil {
		return ae.escalation, ae.escalationPNG
	}
	return nil, nil
}

// escalationBoxLimit caps the text boxes carried in an escalation
// bundle: enough to re-plan on, small enough to journal verbatim.
const escalationBoxLimit = 24

// escalationBoxes compacts the last observation's nodes into
// text-box records for the escalation bundle.
func escalationBoxes(nodes []*Node) []map[string]any {
	boxes := make([]map[string]any, 0, escalationBoxLimit)
	var walk func([]*Node) bool
	walk = func(ns []*Node) bool {
		for _, n := range ns {
			if len(boxes) >= escalationBoxLimit {
				return false
			}
			if n.Label != "" && n.Frame != nil {
				boxes = append(boxes, map[string]any{
					"text": n.Label,
					"x":    n.Frame.X, "y": n.Frame.Y,
					"w": n.Frame.W, "h": n.Frame.H,
				})
			}
			if !walk(n.Children) {
				return false
			}
		}
		return true
	}
	walk(nodes)
	return boxes
}

// evidenceBudget bounds the best-effort escalation screenshot so
// evidence collection cannot meaningfully outlive the wait budget.
const evidenceBudget = 2 * time.Second

// resolveLadder is findElement's ladder orchestration: cycle the rungs
// in order, each attempt bounded by the rung's budget, until a rung
// resolves the element on screen or the wait budget runs out. Matches
// that are transiently off screen keep the wait polling exactly like the
// direct poll path.
func resolveLadder(ctx context.Context, d elementDriver, udid string, p map[string]any, anchor string, lad resolverLadder) (*Node, *Frame, map[string]any, error) {
	pr, err := matcherFromPayload(p)
	if err != nil {
		return nil, nil, nil, err
	}
	refresh, err := boolFlag(p, "refresh", false)
	if err != nil {
		return nil, nil, nil, err
	}
	timeout, interval, err := waitParams(p, defaultWaitTimeout)
	if err != nil {
		return nil, nil, nil, err
	}
	key := breakerKey(ctx, udid, lad.bundle)
	// observing counts the rungs that actually poll an observe source:
	// the map rung is a cache lookup and the template rung is
	// opportunistic, so their presence must not start budget-capping a
	// simulator's single (possibly legitimately slow) ax rung.
	observing := 0
	for _, r := range lad.rungs {
		if r.name() != resolverMapName && r.name() != resolverTemplate {
			observing++
		}
	}
	start := time.Now()
	deadline := start.Add(timeout)
	polls := 0
	skipped := ""
	offScreen := false
	axCounted := false
	axDown := false
	var lastObs observation
	var lastFail error
	trail := map[string]string{}
	// countAXFailure feeds the breaker at most once per action step: a
	// transient blip spanning a few polls inside one step is one failure,
	// not three — only repeated failing steps open the circuit.
	countAXFailure := func() {
		if !axCounted {
			axCounted = true
			lad.breaker.fail(key)
		}
	}
	// ambientErr keeps the old direct-poll wire mapping: an expired
	// ambient deadline (the batch budget) is a 408 timeout, not a 500.
	ambientErr := func(err error) error {
		if errors.Is(err, context.DeadlineExceeded) {
			return timeoutErr("element (%s) did not resolve before the request budget expired (%d poll(s), %s elapsed)",
				pr, polls, time.Since(start).Round(time.Millisecond))
		}
		return err
	}
	for {
		attempted := 0
		// A hard rung failure this cycle fails fast unless some other rung
		// is still alive (observed the screen, or could transiently heal)
		// — so a doomed source reports its actionable error promptly even
		// with the opportunistic template rung registered.
		var hardFail error
		alive := false
		for _, r := range lad.rungs {
			if ctx.Err() != nil {
				return nil, nil, nil, ambientErr(ctx.Err())
			}
			if time.Now().After(deadline) {
				break
			}
			guarded := r.name() == resolverAX && lad.breaker != nil
			if guarded && lad.breaker.isOpen(key) {
				skipped = "ax(circuit-open)"
				trail[r.name()] = "skipped(circuit-open)"
				continue
			}
			if r.name() != resolverMapName {
				// Only observing rungs count as attempts: a map miss
				// cannot substitute for a real source, so it neither
				// keeps a circuit-skipped ladder polling nor blocks the
				// single-source fail-fast below.
				attempted++
			}
			polls++
			budget := time.Until(deadline)
			// The per-rung budget exists so a hung source hands over to
			// the next rung (or trips the breaker) instead of eating the
			// wait budget; a single unguarded observing rung (simulators)
			// has no one to hand over to, so a legitimately slow observe
			// may use the whole remaining budget. The map rung (a cache
			// lookup) and the opportunistic template rung are always
			// bounded — they must hand over on a miss.
			if observing > 1 || guarded || r.name() == resolverMapName || r.name() == resolverTemplate {
				if b := r.budget(); b < budget {
					budget = b
				}
			}
			if r.name() == resolverMapName && observing > 0 {
				// The fast path must never starve the real sources: on a
				// short deadline the map rung gets at most half the
				// remaining budget so an observing rung always gets a
				// turn.
				if half := time.Until(deadline) / 2; half < budget {
					budget = half
				}
			}
			rctx, cancel := context.WithTimeout(ctx, budget)
			hit, obs, rerr := r.resolve(rctx, udid, pr, refresh)
			truncated := rctx.Err() != nil && ctx.Err() == nil
			cancel()
			if rerr == nil {
				if guarded {
					lad.breaker.succeed(key)
					axDown = false
				}
				if obs.viewport != nil && hit.Frame != nil && hit.Frame.W > 0 && hit.Frame.H > 0 {
					if x, y := anchorPoint(hit.Frame, anchor); !pointInViewport(x, y, obs.viewport) {
						offScreen = true
						alive = true
						lastObs = obs
						trail[r.name()] = "off-viewport"
						continue
					}
				}
				res := map[string]any{
					"elapsed_ms": time.Since(start).Milliseconds(),
					"polls":      polls,
					"resolver":   r.name(),
				}
				if skipped != "" {
					res["resolver_skipped"] = skipped
				}
				return hit, obs.viewport, res, nil
			}
			switch {
			case errors.Is(rerr, errRungCannotServe):
				// The rung can never resolve this matcher (no stored
				// template with a matching name): treat it as absent, so a
				// ladder whose remaining rungs are all circuit-skipped
				// still fails fast instead of polling out the budget.
				attempted--
				trail[r.name()] = "no-template"
			case errors.Is(rerr, errAXSourceDown):
				// Transiently-down source: keep polling so the blip can
				// heal; charge the breaker only if it never does (below).
				trail[r.name()] = "source-down"
				axDown = guarded
				offScreen = false
				alive = true
			case errors.Is(rerr, errNoMatch), errors.Is(rerr, errNotYet):
				// An honest miss: source observed (or not ready yet),
				// element absent. Fall through to the next rung.
				if guarded {
					lad.breaker.succeed(key)
					axDown = false
				}
				// Keep the richest screen evidence: a later rung's empty
				// observation (the template rung's miss) must not clobber a
				// real tree / OCR boxes from the same cycle.
				if obs.nodes != nil && (len(obs.nodes) > 0 || lastObs.nodes == nil) {
					lastObs = obs
				}
				offScreen = false
				trail[r.name()] = "no-match"
				// A map-rung miss is a cache miss, not screen evidence: it
				// must not keep a ladder alive whose real source is doomed.
				if r.name() != resolverMapName {
					alive = true
				}
			case truncated && (errors.Is(rerr, context.DeadlineExceeded) || errors.Is(rerr, context.Canceled)):
				// The rung's own budget (or the wait deadline) expired: a
				// snapshot timeout, not a source failure: keep the
				// escalation path.
				trail[r.name()] = "budget-exceeded"
				offScreen = false
				alive = true
				if guarded && !time.Now().After(deadline) {
					countAXFailure()
				}
			default:
				if ctx.Err() != nil {
					// The ambient deadline fired mid-resolve.
					return nil, nil, nil, ambientErr(ctx.Err())
				}
				var ae *Error
				if errors.As(rerr, &ae) && (ae.Code == "bad_request" || ae.Code == "ambiguous_match") {
					// A hard matcher/payload error: more rungs cannot
					// disambiguate it.
					return nil, nil, nil, rerr
				}
				if truncated {
					// The attempt was cut short by the expiring budget and
					// the source surfaced it as its own wrapper (a killed
					// axe run, a mirror observe abort): a timeout, not a
					// source failure — keep the escalation path, exactly
					// like pollUntil's deadline downgrade.
					trail[r.name()] = "budget-exceeded"
					offScreen = false
					alive = true
					if guarded && !time.Now().After(deadline) {
						countAXFailure()
					}
					continue
				}
				if errors.Is(rerr, errResolverConfig) {
					// The source was never contacted — a configuration
					// problem: surface the actionable setup message and
					// leave the breaker alone.
					return nil, nil, nil, rerr
				}
				// Rung source failure (runner death, helper gone).
				trail[r.name()] = "failed"
				offScreen = false
				lastFail = rerr
				hardFail = rerr
				if guarded {
					countAXFailure()
				}

			}
		}
		if hardFail != nil && !alive {
			// No rung can serve: every rung this cycle failed hard, was
			// circuit-skipped, or cannot serve this matcher — fail fast
			// with the source's actionable error (target not booted, tool
			// missing, mirror blocked) instead of retrying doomed sources
			// until the deadline.
			return nil, nil, nil, hardFail
		}
		if attempted == 0 && skipped != "" {
			// Every rung is circuit-skipped: waiting cannot help, fail
			// now instead of burning the budget.
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		wait := interval
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return nil, nil, nil, ambientErr(ctx.Err())
		case <-time.After(wait):
		}
	}
	elapsed := time.Since(start).Round(time.Millisecond)
	if axDown {
		// The AX source was still down when the budget ran out: one
		// breaker failure for the step.
		countAXFailure()
	}
	// No rung ever produced an observation and at least one failed hard:
	// surface that failure (a WDA-only device with a dead runner keeps
	// its actionable "unavailable" message).
	if lastObs.nodes == nil && lastFail != nil && skipped == "" {
		return nil, nil, nil, lastFail
	}
	if offScreen {
		return nil, nil, nil, offViewport("element (%s) matched but stayed outside the viewport for the whole %s budget (%d poll(s), %s elapsed); scroll it into view first",
			pr, timeout, polls, elapsed)
	}
	wire := timeoutErr("element (%s) did not resolve within the %s budget (rungs: %s; %d poll(s), %s elapsed)%s",
		pr, timeout, trailSummary(lad.rungs, trail), polls, elapsed,
		offscreenHint(pr, lastObs.nodes, lastObs.viewport))
	wire.escalation = map[string]any{
		"rungs":      trailSummary(lad.rungs, trail),
		"text_boxes": escalationBoxes(lastObs.nodes),
	}
	if skipped != "" {
		wire.escalation["resolver_skipped"] = skipped
	}
	if cap, ok := d.(evidenceCapturer); ok && ctx.Err() == nil && hasEvidenceSink(ctx) {
		ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), evidenceBudget)
		defer cancel()
		if png, err := cap.captureEvidence(ectx, udid); err == nil {
			wire.escalationPNG = png
		}
	}
	return nil, nil, nil, wire
}

// trailSummary renders the per-rung outcomes in ladder order.
func trailSummary(rungs []resolver, trail map[string]string) string {
	s := ""
	for _, r := range rungs {
		outcome, ok := trail[r.name()]
		if !ok {
			outcome = "not-attempted"
		}
		if s != "" {
			s += ", "
		}
		s += fmt.Sprintf("%s=%s", r.name(), outcome)
	}
	return s
}
