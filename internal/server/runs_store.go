package server

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// Run resources live in memory only: the durable record of a run is its
// journal (run ID = lease ID). The store keeps the most recent finished
// runs around so async callers can poll results after completion.
const (
	// maxConcurrentRuns caps runs executing at once; further POSTs are
	// refused with 503 overloaded (a run holds a lease + boot slot, so
	// unbounded concurrency would just queue on those anyway).
	maxConcurrentRuns = 4
	// maxFinishedRuns bounds retained finished runs (oldest pruned first).
	maxFinishedRuns = 64
)

// runStore tracks run resources and the concurrency cap.
type runStore struct {
	mu     sync.Mutex
	runs   map[string]*runHandle
	active int
}

// runHandle is one run's mutable state plus its completion signal.
type runHandle struct {
	mu   sync.Mutex
	run  proto.Run
	done chan struct{}
}

// snapshot returns a copy of the run for serving (steps deep-copied so a
// concurrent append cannot race the encoder).
func (h *runHandle) snapshot() proto.Run {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.run
	r.Steps = append([]proto.RunStepResult(nil), h.run.Steps...)
	return r
}

// update mutates the run under its lock.
func (h *runHandle) update(fn func(r *proto.Run)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(&h.run)
}

func newRunStore() *runStore {
	return &runStore{runs: make(map[string]*runHandle)}
}

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "run_" + hex.EncodeToString(b)
}

// create registers a new pending run, enforcing the concurrency cap.
// It returns nil when the daemon is at capacity.
func (s *runStore) create(spec proto.RunSpec, agentID string) *runHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active >= maxConcurrentRuns {
		return nil
	}
	s.active++
	h := &runHandle{
		run: proto.Run{
			ID:        newRunID(),
			State:     proto.RunPending,
			Spec:      spec,
			AgentID:   agentID,
			CreatedAt: time.Now().UTC(),
		},
		done: make(chan struct{}),
	}
	s.runs[h.run.ID] = h
	s.pruneLocked()
	return h
}

// finish marks a run's terminal state and releases its concurrency slot.
func (s *runStore) finish(h *runHandle) {
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	close(h.done)
}

// get returns the handle for a run ID.
func (s *runStore) get(id string) (*runHandle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.runs[id]
	return h, ok
}

// list snapshots every retained run, newest first.
func (s *runStore) list() []proto.Run {
	s.mu.Lock()
	handles := make([]*runHandle, 0, len(s.runs))
	for _, h := range s.runs {
		handles = append(handles, h)
	}
	s.mu.Unlock()
	out := make([]proto.Run, 0, len(handles))
	for _, h := range handles {
		out = append(out, h.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// pruneLocked drops the oldest finished runs beyond maxFinishedRuns.
// Callers hold s.mu.
func (s *runStore) pruneLocked() {
	var finished []*runHandle
	for _, h := range s.runs {
		select {
		case <-h.done:
			finished = append(finished, h)
		default:
		}
	}
	if len(finished) <= maxFinishedRuns {
		return
	}
	sort.Slice(finished, func(i, j int) bool {
		return finished[i].snapshot().CreatedAt.Before(finished[j].snapshot().CreatedAt)
	})
	for _, h := range finished[:len(finished)-maxFinishedRuns] {
		delete(s.runs, h.snapshot().ID)
	}
}
