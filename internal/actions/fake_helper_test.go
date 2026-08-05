package actions

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var errDeliveredBoom = errors.New("died after the request was written")

// fakeHelper is an in-memory Helper for warm-path tests.
type fakeHelper struct {
	mu     sync.Mutex
	udid   string
	calls  []string // "op" per call
	closed bool

	// results maps op -> canned result.
	results map[string]map[string]any
	// errs maps op -> action-level error message (returned as *Error).
	errs map[string]string
	// transportFails is how many remaining calls fail with a transport
	// error before succeeding.
	transportFails int
	// deliveredFails is how many remaining calls fail with a Delivered
	// transport error (the request was written before the helper died).
	deliveredFails int
	// hang makes Call block until the context is cancelled.
	hang bool
	// delays maps op -> how long the call takes before responding.
	delays map[string]time.Duration
}

func (f *fakeHelper) Call(ctx context.Context, op string, _ map[string]any) (map[string]any, error) {
	f.mu.Lock()
	if f.hang {
		f.mu.Unlock()
		<-ctx.Done()
		return nil, transportErr("call cancelled: %v", ctx.Err())
	}
	defer f.mu.Unlock()
	f.calls = append(f.calls, op)
	if d, ok := f.delays[op]; ok {
		time.Sleep(d)
	}
	if f.closed {
		return nil, transportErr("helper closed")
	}
	if f.transportFails > 0 {
		f.transportFails--
		return nil, transportErr("boom")
	}
	if f.deliveredFails > 0 {
		f.deliveredFails--
		return nil, &TransportError{Err: errDeliveredBoom, Delivered: true}
	}
	if msg, ok := f.errs[op]; ok {
		return nil, &Error{Code: "internal", Message: msg}
	}
	if res, ok := f.results[op]; ok {
		return res, nil
	}
	return map[string]any{}, nil
}

func (f *fakeHelper) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeHelper) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeFactory builds fakeHelpers and records every spawn.
type fakeFactory struct {
	mu       sync.Mutex
	spawned  []*fakeHelper
	attempts int
	// spawnErrs fails the next N spawns.
	spawnErrs int
	// configure customizes each new helper.
	configure func(h *fakeHelper)
}

func (ff *fakeFactory) factory(udid string) (Helper, error) {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	ff.attempts++
	if ff.spawnErrs > 0 {
		ff.spawnErrs--
		return nil, transportErr("spawn failed")
	}
	h := &fakeHelper{udid: udid, results: map[string]map[string]any{}, errs: map[string]string{}}
	if ff.configure != nil {
		ff.configure(h)
	}
	ff.spawned = append(ff.spawned, h)
	return h, nil
}

func (ff *fakeFactory) spawnCount() int {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return len(ff.spawned)
}

func (ff *fakeFactory) attemptCount() int {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return ff.attempts
}

func (ff *fakeFactory) helper(i int) *fakeHelper {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if i >= len(ff.spawned) {
		panic(fmt.Sprintf("no helper %d (spawned %d)", i, len(ff.spawned)))
	}
	return ff.spawned[i]
}
