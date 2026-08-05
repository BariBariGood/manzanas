package actions

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPoolReusesHelperPerUDID(t *testing.T) {
	ff := &fakeFactory{}
	p := NewPool(ff.factory, PoolConfig{})
	defer p.Close()

	for i := 0; i < 3; i++ {
		if _, err := p.Call(context.Background(), "U1", "ping", nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := ff.spawnCount(); got != 1 {
		t.Fatalf("spawned %d helpers, want 1", got)
	}
	if got := ff.helper(0).callCount(); got != 3 {
		t.Fatalf("helper served %d calls, want 3", got)
	}
}

func TestPoolRestartsOnTransportFailure(t *testing.T) {
	ff := &fakeFactory{}
	ff.configure = func(h *fakeHelper) {
		if len(ff.spawned) == 0 { // only the first helper is flaky
			h.transportFails = 1
		}
	}
	p := NewPool(ff.factory, PoolConfig{})
	defer p.Close()

	if _, err := p.Call(context.Background(), "U1", "ping", nil); err != nil {
		t.Fatalf("call should succeed after restart: %v", err)
	}
	if got := ff.spawnCount(); got != 2 {
		t.Fatalf("spawned %d helpers, want 2 (restart)", got)
	}
	if !ff.helper(0).closed {
		t.Fatal("failed helper was not closed")
	}
}

func TestPoolReturnsErrorWhenRestartAlsoFails(t *testing.T) {
	ff := &fakeFactory{configure: func(h *fakeHelper) { h.transportFails = 5 }}
	p := NewPool(ff.factory, PoolConfig{})
	defer p.Close()

	_, err := p.Call(context.Background(), "U1", "ping", nil)
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want TransportError, got %v", err)
	}
	if p.WarmCount() != 0 {
		t.Fatalf("broken helper should be evicted, warm=%d", p.WarmCount())
	}
}

func TestPoolActionErrorDoesNotRestart(t *testing.T) {
	ff := &fakeFactory{configure: func(h *fakeHelper) { h.errs["tap"] = "no such point" }}
	p := NewPool(ff.factory, PoolConfig{})
	defer p.Close()

	_, err := p.Call(context.Background(), "U1", "tap", nil)
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *Error, got %v", err)
	}
	if got := ff.spawnCount(); got != 1 {
		t.Fatalf("action error should not restart helper (spawned %d)", got)
	}
	if p.WarmCount() != 1 {
		t.Fatal("helper should stay resident after an action error")
	}
}

func TestPoolEvictsLRUAtCapacity(t *testing.T) {
	ff := &fakeFactory{}
	p := NewPool(ff.factory, PoolConfig{MaxTargets: 2})
	defer p.Close()

	now := time.Now()
	p.clock = func() time.Time { return now }
	mustCall(t, p, "U1")
	now = now.Add(time.Second)
	mustCall(t, p, "U2")
	now = now.Add(time.Second)
	mustCall(t, p, "U3") // evicts U1

	if p.WarmCount() != 2 {
		t.Fatalf("warm=%d, want 2", p.WarmCount())
	}
	if !ff.helper(0).closed {
		t.Fatal("LRU helper (U1) was not closed")
	}
	if ff.helper(1).closed || ff.helper(2).closed {
		t.Fatal("newer helpers must stay resident")
	}
}

func TestPoolReapsIdleHelpers(t *testing.T) {
	ff := &fakeFactory{}
	p := NewPool(ff.factory, PoolConfig{IdleTTL: time.Minute})
	defer p.Close()

	now := time.Now()
	p.clock = func() time.Time { return now }
	mustCall(t, p, "U1")

	now = now.Add(2 * time.Minute)
	p.reapIdle()

	if p.WarmCount() != 0 {
		t.Fatalf("idle helper not reaped, warm=%d", p.WarmCount())
	}
	if !ff.helper(0).closed {
		t.Fatal("idle helper was not closed")
	}
}

func TestPoolCloseShutsDownHelpers(t *testing.T) {
	ff := &fakeFactory{}
	p := NewPool(ff.factory, PoolConfig{})
	mustCall(t, p, "U1")
	mustCall(t, p, "U2")
	p.Close()

	if !ff.helper(0).closed || !ff.helper(1).closed {
		t.Fatal("Close must shut down every helper")
	}
	if _, err := p.Call(context.Background(), "U3", "ping", nil); err == nil {
		t.Fatal("calls after Close must fail")
	}
}

func TestPoolTimesOutWedgedHelper(t *testing.T) {
	ff := &fakeFactory{configure: func(h *fakeHelper) { h.hang = true }}
	p := NewPool(ff.factory, PoolConfig{OpTimeout: 20 * time.Millisecond})
	defer p.Close()

	_, err := p.Call(context.Background(), "U1", "ping", nil)
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want TransportError on op timeout, got %v", err)
	}
	if got := ff.spawnCount(); got != 1 {
		t.Fatalf("timeout must not restart+replay (spawned %d)", got)
	}
	if p.WarmCount() != 0 {
		t.Fatalf("wedged helper should be evicted, warm=%d", p.WarmCount())
	}
	if !ff.helper(0).closed {
		t.Fatal("wedged helper was not closed")
	}
}

func TestPoolSpawnFailureCoolsDown(t *testing.T) {
	ff := &fakeFactory{spawnErrs: 1}
	p := NewPool(ff.factory, PoolConfig{SpawnCooldown: time.Minute})
	defer p.Close()

	now := time.Now()
	p.clock = func() time.Time { return now }

	if _, err := p.Call(context.Background(), "U1", "ping", nil); err == nil {
		t.Fatal("first call should fail (spawn error)")
	}
	if _, err := p.Call(context.Background(), "U1", "ping", nil); err == nil {
		t.Fatal("call during cooldown should fail without spawning")
	}
	if got := ff.attemptCount(); got != 1 {
		t.Fatalf("cooldown must skip the factory (attempts=%d, want 1)", got)
	}

	now = now.Add(2 * time.Minute)
	mustCall(t, p, "U1")
	if got := ff.attemptCount(); got != 2 {
		t.Fatalf("cooldown expiry should retry warm (attempts=%d, want 2)", got)
	}
}

func TestPoolDoesNotReplayDeliveredFailure(t *testing.T) {
	ff := &fakeFactory{configure: func(h *fakeHelper) { h.deliveredFails = 1 }}
	p := NewPool(ff.factory, PoolConfig{})
	defer p.Close()

	_, err := p.Call(context.Background(), "U1", "tap", nil)
	var te *TransportError
	if !errors.As(err, &te) || !te.Delivered {
		t.Fatalf("want Delivered TransportError, got %v", err)
	}
	if got := ff.spawnCount(); got != 1 {
		t.Fatalf("delivered failure must not restart+replay (spawned %d)", got)
	}
	if p.WarmCount() != 0 {
		t.Fatalf("broken helper should be evicted, warm=%d", p.WarmCount())
	}
}

func TestPoolCrashLoopCoolsDown(t *testing.T) {
	ff := &fakeFactory{configure: func(h *fakeHelper) { h.transportFails = 99 }}
	p := NewPool(ff.factory, PoolConfig{SpawnCooldown: time.Minute})
	defer p.Close()

	now := time.Now()
	p.clock = func() time.Time { return now }

	if _, err := p.Call(context.Background(), "U1", "ping", nil); err == nil {
		t.Fatal("crash-looping helper should fail")
	}
	spawnsAfterFirst := ff.spawnCount()
	if _, err := p.Call(context.Background(), "U1", "ping", nil); err == nil {
		t.Fatal("call during cooldown should fail")
	}
	if got := ff.spawnCount(); got != spawnsAfterFirst {
		t.Fatalf("crash loop must cool down, not respawn (spawned %d, want %d)", got, spawnsAfterFirst)
	}
}

func TestPoolCallerCancelDoesNotCoolDown(t *testing.T) {
	ff := &fakeFactory{configure: func(h *fakeHelper) { h.hang = true }}
	p := NewPool(ff.factory, PoolConfig{SpawnCooldown: time.Minute, OpTimeout: 100 * time.Millisecond})
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := p.Call(ctx, "U1", "ping", nil); err == nil {
		t.Fatal("cancelled call should fail")
	}

	if _, err := p.Call(context.Background(), "U1", "ping", nil); err == nil {
		t.Fatal("second call should still try warm (hang helper)")
	}
	if got := ff.attemptCount(); got != 2 {
		t.Fatalf("caller cancel must not cool the target down (attempts=%d, want 2)", got)
	}
}

func TestPoolDropClosesHelper(t *testing.T) {
	ff := &fakeFactory{}
	p := NewPool(ff.factory, PoolConfig{})
	defer p.Close()

	mustCall(t, p, "U1")
	p.Drop("U1")

	if !ff.helper(0).closed {
		t.Fatal("Drop must close the resident helper")
	}
	if p.WarmCount() != 0 {
		t.Fatalf("warm=%d, want 0 after Drop", p.WarmCount())
	}
	mustCall(t, p, "U1")
	if got := ff.spawnCount(); got != 2 {
		t.Fatalf("next call should spawn fresh (spawned %d, want 2)", got)
	}
}

func mustCall(t *testing.T, p *Pool, udid string) {
	t.Helper()
	if _, err := p.Call(context.Background(), udid, "ping", nil); err != nil {
		t.Fatalf("call %s: %v", udid, err)
	}
}
