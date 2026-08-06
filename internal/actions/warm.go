package actions

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// WarmBackend implements Backend over a pool of resident simbridge
// helpers, eliminating the per-command process spawn + FBSimulatorControl
// bootstrap of the cold AXe path. Kinds the helper does not implement, and
// any transport failure, fall through to the wrapped cold backend, so the
// warm layer is transparent: it only ever makes actions faster.
type WarmBackend struct {
	pool *Pool
	cold Backend
	// coldAXe is true when the wrapped cold backend can actually observe
	// (an AXe binary exists); a refresh:true observe is only re-routed
	// cold when it can be served there.
	coldAXe bool
	log     *slog.Logger
	// warmOps maps an action kind to its simbridge translation; kinds not
	// present are always dispatched cold.
	warmOps map[string]warmOp
}

// warmOp translates one action kind onto a simbridge call.
type warmOp struct {
	// op is the simbridge operation name.
	op string
	// encode validates the action payload and produces helper args.
	encode func(p map[string]any) (map[string]any, error)
	// decode turns the helper result into the action result payload.
	decode func(res map[string]any, p map[string]any) (map[string]any, error)
	// idempotent ops are safe to re-run cold even when a failed warm
	// attempt may already have executed (reads like observe); HID inputs
	// are not, or one API request could land two taps.
	idempotent bool
}

// NewWarm wraps a cold backend with the warm helper pool. The factory is
// typically NewProcHelper bound to a simbridge binary path.
func NewWarm(cold Backend, factory HelperFactory, cfg PoolConfig) *WarmBackend {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	b := &WarmBackend{
		pool: NewPool(factory, cfg),
		cold: cold,
		log:  cfg.Logger,
	}
	b.warmOps = map[string]warmOp{
		"tap":   {op: "tap", encode: encodeTap, decode: echoArgs("x", "y")},
		"swipe": {op: "swipe", encode: encodeSwipe, decode: echoArgs("start_x", "start_y", "end_x", "end_y")},
		"button": {op: "button", encode: encodeButton, decode: func(_ map[string]any, p map[string]any) (map[string]any, error) {
			return map[string]any{"button": p["name"]}, nil
		}},
		"key":          {op: "key", encode: encodeKey, decode: echoArgs("keycode")},
		"key_sequence": {op: "key_sequence", encode: encodeKeySequence, decode: decodeKeySequence},
		"observe":      {op: "describe_ui", encode: encodeObserve, decode: nil, idempotent: true}, // custom path below
	}
	// A non-AXe cold backend is assumed able to observe; only a known
	// AXe-less host keeps refresh observes on the warm helper.
	b.coldAXe = true
	if ab, ok := cold.(*AXeBackend); ok {
		b.coldAXe = ab.AXeAvailable()
		// Accelerate the cold backend's per-poll a11y reads (wait_for_element,
		// wait_tree_stable) through the resident helper.
		ab.SetWarmObserver(b.observePoll)
		// Accelerate the composite element actions' tap through the
		// resident helper too, so tap_element isn't slower than the
		// observe+tap flow it replaces on warm hosts.
		ab.SetWarmHID(b.hidOp)
	}
	return b
}

// hidOp services one HID op for the cold backend's composite actions.
// A helper predating an op rejects it as "unknown op"; that maps to
// errColdOnly so the caller runs the input on the cold CLI instead.
func (b *WarmBackend) hidOp(ctx context.Context, udid, op string, args map[string]any) error {
	_, err := b.pool.Call(ctx, udid, op, args)
	var ae *Error
	if errors.As(err, &ae) && strings.HasPrefix(ae.Message, "unknown op") {
		return errColdOnly
	}
	return err
}

// Close shuts down all warm helpers.
func (b *WarmBackend) Close() { b.pool.Close() }

// Invalidate drops the resident helper for a UDID. Call it when the
// simulator's state changes underneath the helper (lease-end reset,
// erase, reboot) so the next action bootstraps a fresh connection.
func (b *WarmBackend) Invalidate(udid string) { b.pool.Drop(udid) }

// WarmCount reports resident helpers (diagnostics).
func (b *WarmBackend) WarmCount() int { return b.pool.WarmCount() }

// Dispatch implements Backend.
func (b *WarmBackend) Dispatch(ctx context.Context, udid string, req proto.ActionRequest) (proto.ActionResult, error) {
	w, ok := b.warmOps[req.Kind]
	if !ok {
		return b.cold.Dispatch(ctx, udid, req)
	}
	// Opt-in a11y evidence around HID actions, mirroring the cold path:
	// best-effort and time-bounded (axHashTimeout each) via the resident
	// helper, so warm and cold produce the same ax_before/ax_after fields.
	wantAX := false
	if hidKinds[req.Kind] {
		v, err := boolFlag(req.Payload, "ax_hashes", false)
		if err != nil {
			return proto.ActionResult{}, err
		}
		wantAX = v
	}
	var axBefore string
	if wantAX {
		axBefore = b.hashTree(ctx, udid)
	}
	res, err := b.dispatchWarm(ctx, udid, req, w)
	if errors.Is(err, errColdOnly) {
		return b.cold.Dispatch(ctx, udid, req)
	}
	var te *TransportError
	if errors.As(err, &te) {
		if te.Delivered && !w.idempotent {
			// The input may already have landed on the simulator; re-running
			// it cold could apply it twice, so surface the failure instead.
			b.log.Warn("warm helper failed after input may have been delivered; not retrying",
				"kind", req.Kind, "udid", udid, "err", err)
			return proto.ActionResult{}, unavailable("warm helper failed mid-%s; the input may or may not have been delivered", req.Kind)
		}
		b.log.Warn("warm path unavailable; falling back to cold backend",
			"kind", req.Kind, "udid", udid, "err", err)
		return b.cold.Dispatch(ctx, udid, req)
	}
	if err != nil {
		return proto.ActionResult{}, err
	}
	if wantAX {
		if res == nil {
			res = map[string]any{}
		}
		if axBefore != "" {
			res["ax_before"] = axBefore
		}
		if h := b.hashTree(ctx, udid); h != "" {
			res["ax_after"] = h
		}
	}
	return proto.ActionResult{OK: true, Result: res}, nil
}

// warmAXHashTimeout is the evidence budget for one warm tree hash
// (variable so tests can shrink it).
var warmAXHashTimeout = axHashTimeout

// hashTree returns the current a11y tree hash via the warm helper, or ""
// on any failure/timeout (evidence hashing never fails the action).
// The helper services one op at a time over a serial stream, so a
// deadline that cancels a describe_ui mid-call would desync the stream
// and cost the resident helper. Instead the call runs to completion on
// its own (bounded by the pool's op timeout) and a hash that misses the
// evidence budget is simply abandoned; the pool's per-target lock keeps
// the following op ordered behind it.
func (b *WarmBackend) hashTree(ctx context.Context, udid string) string {
	ch := make(chan string, 1)
	go func() {
		res, err := b.pool.Call(context.WithoutCancel(ctx), udid, "describe_ui", nil)
		if err != nil {
			ch <- ""
			return
		}
		raw, _ := res["raw"].(string)
		nodes, err := CompactTree([]byte(raw))
		if err != nil {
			ch <- ""
			return
		}
		ch <- TreeHash(nodes)
	}()
	select {
	case h := <-ch:
		return h
	case <-time.After(warmAXHashTimeout):
		return ""
	case <-ctx.Done():
		return ""
	}
}

// observePoll is the warm single-poll observer plugged into the cold
// backend for the wait_* loops: one describe_ui on the resident helper,
// compacted; transient bridge races come back as errNotYet. Like
// hashTree, a poll that outlives the caller's deadline is abandoned
// rather than cancelled, so a short wait budget cannot desync the
// helper stream and cost the resident helper.
func (b *WarmBackend) observePoll(ctx context.Context, udid string) (observation, error) {
	type callOut struct {
		res map[string]any
		err error
	}
	ch := make(chan callOut, 1)
	go func() {
		res, err := b.pool.Call(context.WithoutCancel(ctx), udid, "describe_ui", nil)
		ch <- callOut{res, err}
	}()
	var res map[string]any
	var err error
	select {
	case o := <-ch:
		res, err = o.res, o.err
	case <-ctx.Done():
		return observation{}, ctx.Err()
	}
	if err != nil {
		if isTransientA11yError(err) {
			return observation{}, errNotYet
		}
		return observation{}, err
	}
	raw, _ := res["raw"].(string)
	nodes, err := CompactTree([]byte(raw))
	if err != nil || len(nodes) == 0 {
		return observation{}, errNotYet
	}
	return observation{nodes: nodes, viewport: rawViewport([]byte(raw))}, nil
}

func (b *WarmBackend) dispatchWarm(ctx context.Context, udid string, req proto.ActionRequest, w warmOp) (map[string]any, error) {
	args, err := w.encode(req.Payload)
	if err != nil {
		return nil, err
	}
	if req.Kind == "observe" {
		return b.warmObserve(ctx, udid, req.Payload)
	}
	res, err := b.pool.Call(ctx, udid, w.op, args)
	if err != nil {
		return nil, err
	}
	return w.decode(res, req.Payload)
}

// warmObserve mirrors the cold backend's observe semantics (retry while
// the a11y bridge attaches, compact, hash) over the warm describe_ui op.
// A refresh:true payload runs the observe cold instead: a freshly spawned
// AXe process opens a fresh accessibility connection, which cannot serve
// the stale snapshot a long-lived resident helper occasionally does (a
// frontmost modal/sheet missing from the tree for minutes).
func (b *WarmBackend) warmObserve(ctx context.Context, udid string, p map[string]any) (map[string]any, error) {
	refresh, err := boolFlag(p, "refresh", false)
	if err != nil {
		return nil, err
	}
	if refresh && b.coldAXe {
		return nil, errColdOnly
	}
	includeRaw, err := boolFlag(p, "include_raw", false)
	if err != nil {
		return nil, err
	}
	raw, nodes, err := b.warmObserveTree(ctx, udid)
	if err != nil {
		return nil, err
	}
	if nodes == nil {
		nodes = []*Node{}
	}
	res := map[string]any{
		"tree": nodes,
		"hash": TreeHash(nodes),
	}
	if len(nodes) == 0 {
		// Same retryable signal as the cold path (see handleObserve).
		res["detail"] = "empty_tree"
	}
	if includeRaw {
		res["raw"] = string(raw)
	}
	return res, nil
}

func (b *WarmBackend) warmObserveTree(ctx context.Context, udid string) ([]byte, []*Node, error) {
	var lastErr error
	empties := 0
	for attempt := 0; attempt < observeRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * observeBackoff):
			}
		}
		res, err := b.pool.Call(ctx, udid, "describe_ui", nil)
		if err != nil {
			var te *TransportError
			if errors.As(err, &te) {
				return nil, nil, err // let Dispatch fall back cold
			}
			lastErr = err
			if !isTransientA11yError(err) {
				return nil, nil, err
			}
			continue
		}
		raw, _ := res["raw"].(string)
		nodes, err := CompactTree([]byte(raw))
		if err != nil {
			lastErr = err
			continue
		}
		if len(nodes) > 0 || empties >= emptyTreeRetries {
			return []byte(raw), nodes, nil
		}
		empties++
	}
	if e, ok := lastErr.(*Error); ok && e.Code == "internal" {
		return nil, nil, unavailable("accessibility bridge not ready after %d attempts; retry later: %s", observeRetries, e.Message)
	}
	return nil, nil, lastErr
}

// --- per-kind payload translation ---

func encodeTap(p map[string]any) (map[string]any, error) {
	x, err := coordField(p, "x")
	if err != nil {
		return nil, err
	}
	y, err := coordField(p, "y")
	if err != nil {
		return nil, err
	}
	return map[string]any{"x": x, "y": y}, nil
}

func encodeSwipe(p map[string]any) (map[string]any, error) {
	args := map[string]any{}
	for _, k := range []string{"start_x", "start_y", "end_x", "end_y"} {
		v, err := coordField(p, k)
		if err != nil {
			return nil, err
		}
		args[k] = v
	}
	if d, ok := p["duration_seconds"]; ok {
		dv, err := toNum(d)
		if err != nil || dv <= 0 {
			return nil, badRequest("duration_seconds must be a positive number")
		}
		args["duration"] = dv
	}
	return args, nil
}

// errColdOnly marks a payload the warm helper cannot service; the action
// is dispatched on the cold backend instead.
var errColdOnly = errors.New("cold-only payload")

func encodeButton(p map[string]any) (map[string]any, error) {
	name, _ := p["name"].(string)
	if !validButtons[name] {
		return nil, badRequest("button requires payload field %q, one of: %s", "name", buttonNames())
	}
	return map[string]any{"name": name}, nil
}

// keycodeInRange bounds HID keycodes on both backends: the Swift helper's
// UInt32 conversion traps (aborting the helper) on negative or oversized
// values, and AXe's flag parser misreads a negative argv element.
func keycodeInRange(v float64) bool {
	return v >= 0 && v <= math.MaxUint32 && v == math.Trunc(v)
}

func encodeKey(p map[string]any) (map[string]any, error) {
	code, err := numField(p, "keycode")
	if err != nil {
		return nil, err
	}
	if !keycodeInRange(code) {
		return nil, badRequest("keycode out of range")
	}
	args := map[string]any{"keycode": code}
	if d, ok := p["duration_seconds"]; ok {
		dv, err := toNum(d)
		if err != nil || dv <= 0 {
			return nil, badRequest("duration_seconds must be a positive number")
		}
		args["duration"] = dv
	}
	return args, nil
}

func encodeKeySequence(p map[string]any) (map[string]any, error) {
	raw, ok := p["keycodes"].([]any)
	if !ok || len(raw) == 0 {
		return nil, badRequest("key_sequence requires a non-empty array payload field %q", "keycodes")
	}
	codes := make([]any, 0, len(raw))
	for i, r := range raw {
		v, err := toNum(r)
		if err != nil {
			return nil, badRequest("keycodes[%d] must be a number", i)
		}
		if !keycodeInRange(v) {
			return nil, badRequest("keycodes[%d] out of range", i)
		}
		codes = append(codes, v)
	}
	return map[string]any{"keycodes": codes}, nil
}

func encodeObserve(map[string]any) (map[string]any, error) { return nil, nil }

func decodeKeySequence(_ map[string]any, p map[string]any) (map[string]any, error) {
	raw, _ := p["keycodes"].([]any)
	return map[string]any{"count": len(raw)}, nil
}

// echoArgs mirrors the cold backend's result shape: it echoes the named
// payload fields back (numeric fields normalized via toNum).
func echoArgs(keys ...string) func(map[string]any, map[string]any) (map[string]any, error) {
	return func(_ map[string]any, p map[string]any) (map[string]any, error) {
		out := make(map[string]any, len(keys))
		for _, k := range keys {
			v := p[k]
			if n, err := toNum(v); err == nil {
				out[k] = n
			} else {
				out[k] = v
			}
		}
		return out, nil
	}
}
