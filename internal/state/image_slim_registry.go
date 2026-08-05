package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// slimRegistry is a small JSON index (mirrors imageIndex) mapping the
// UDIDs of slimmed sims — stamped from a slim golden image — to the
// services that must stay disabled on them. `simctl erase` wipes the
// per-UDID launchctl disable config, so the engine re-applies the
// recorded set after every erase manzanasd performs.
type slimRegistry struct {
	mu   sync.Mutex
	path string
}

type slimRegistryFile struct {
	// Sims maps udid -> disabled services.
	Sims map[string][]string `json:"sims"`
}

func newSlimRegistry(path string) *slimRegistry {
	return &slimRegistry{path: path}
}

func (r *slimRegistry) load() (slimRegistryFile, error) {
	f := slimRegistryFile{Sims: map[string][]string{}}
	b, err := os.ReadFile(r.path)
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("read slim registry: %w", err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("parse slim registry %s: %w", r.path, err)
	}
	if f.Sims == nil {
		f.Sims = map[string][]string{}
	}
	return f, nil
}

func (r *slimRegistry) save(f slimRegistryFile) error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// RecordBatch stores the same disabled-service set for every udid in a
// single write, so a batch can never end up half-tracked.
func (r *slimRegistry) RecordBatch(udids []string, services []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return err
	}
	for _, udid := range udids {
		f.Sims[udid] = services
	}
	return r.save(f)
}

// Lookup returns the recorded services for udid, if any.
func (r *slimRegistry) Lookup(udid string) ([]string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return nil, false, err
	}
	svcs, ok := f.Sims[udid]
	return svcs, ok, nil
}

// ForgetBatch drops every udid in a single write.
func (r *slimRegistry) ForgetBatch(udids []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return err
	}
	changed := false
	for _, udid := range udids {
		if _, ok := f.Sims[udid]; ok {
			delete(f.Sims, udid)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.save(f)
}

// Prune drops every entry whose udid is not in exists — sims deleted out
// of band (operator delete, stamp rollback on a crashed daemon) are never
// erased again, so their entries would otherwise persist forever.
func (r *slimRegistry) Prune(exists map[string]bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return err
	}
	changed := false
	for udid := range f.Sims {
		if !exists[udid] {
			delete(f.Sims, udid)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.save(f)
}

// Forget drops udid from the registry (best effort).
func (r *slimRegistry) Forget(udid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return err
	}
	if _, ok := f.Sims[udid]; !ok {
		return nil
	}
	delete(f.Sims, udid)
	return r.save(f)
}

// ReapplySlim re-applies the recorded disable set to udid after an erase:
// boot, ensure every recorded service is disabled (re-applying and
// re-verifying), shut down again. Returns false when udid was never
// stamped from a slim image (nothing to do). A sim that vanished out of
// band is forgotten rather than failed.
func (s *ImageStore) ReapplySlim(ctx context.Context, udid string) (bool, error) {
	if err := validUDID(udid); err != nil {
		return false, err
	}
	services, ok, err := s.slimmed.Lookup(udid)
	if err != nil {
		return false, err
	}
	if !ok || len(services) == 0 {
		return false, nil
	}
	if _, err := deviceState(ctx, s.run, udid); errors.Is(err, errDeviceNotFound) {
		_ = s.slimmed.Forget(udid)
		return false, nil
	} else if err != nil {
		return false, err
	}
	if _, err := s.run.Simctl(ctx, "boot", udid); err != nil {
		// A cancelled/timed-out boot may still leave the sim powered on
		// (CoreSimulator finishes the boot after the CLI dies); shut it
		// back down on a detached context like the success path does.
		bctx, bcancel := cleanupCtx(ctx)
		_, _ = s.run.Simctl(bctx, "shutdown", udid)
		bcancel()
		return true, fmt.Errorf("re-slim %s: %w", udid, err)
	}
	slimErr := ensureDisabled(ctx, s.run, udid, services)
	sctx, cancel := cleanupCtx(ctx)
	defer cancel()
	if _, err := s.run.Simctl(sctx, "shutdown", udid); err != nil &&
		!strings.Contains(err.Error(), "current state: Shutdown") && slimErr == nil {
		return true, fmt.Errorf("re-slim %s: %w", udid, err)
	}
	if slimErr != nil {
		return true, fmt.Errorf("re-slim %s: %w", udid, slimErr)
	}
	return true, nil
}
