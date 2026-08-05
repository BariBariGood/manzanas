package warm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
)

// ErrNotRunning means Park found no launchd_sim tree for the UDID (the
// simulator is not booted).
var ErrNotRunning = errors.New("simulator has no running launchd_sim tree")

// Parker suspends and resumes simulator process trees. The PID list is
// cached at park time so Thaw is a straight cached-PID SIGCONT (~25ms on
// M3, ~225ms on Intel) instead of a ps-walk (0.4-2s M3, ~6.5s Intel).
type Parker struct {
	host Host

	mu     sync.Mutex
	parked map[string][]int // udid -> tree cached at park time
	// owned remembers every tree this daemon ever parked (pid -> command
	// line as recorded), surviving thaw/shutdown, so the orphan reaper
	// only touches trees we own and never a recycled PID.
	owned map[string]map[int]string
}

// NewParker builds a Parker over the given host.
func NewParker(host Host) *Parker {
	return &Parker{host: host, parked: make(map[string][]int), owned: make(map[string]map[int]string)}
}

// Park SIGSTOPs the simulator's process tree, root (launchd_sim) first so
// nothing respawns mid-stop, and caches the PID list for Thaw. Idempotent:
// parking a parked sim re-sends STOP (harmless) and refreshes the cache.
func (p *Parker) Park(ctx context.Context, udid string) error {
	procs, err := p.host.Processes(ctx)
	if err != nil {
		return err
	}
	tree := simTree(procs, udid)
	if len(tree) == 0 {
		return fmt.Errorf("park %s: %w", udid, ErrNotRunning)
	}
	for _, pid := range tree {
		if err := p.host.Signal(pid, syscall.SIGSTOP); err != nil {
			// Roll back: never leave a half-stopped tree behind.
			for _, q := range tree {
				_ = p.host.Signal(q, syscall.SIGCONT)
			}
			return fmt.Errorf("park %s: SIGSTOP %d: %w", udid, pid, err)
		}
	}
	cmds := make(map[int]string, len(procs))
	for _, pr := range procs {
		cmds[pr.PID] = pr.Command
	}
	p.mu.Lock()
	p.parked[udid] = tree
	// Merge into the ownership ledger rather than replacing it: a
	// leftover process from the previous cycle that a reap couldn't kill
	// must stay reapable. Prune entries whose PID is gone or was reused
	// by a different command.
	owned := make(map[int]string, len(tree)+len(p.owned[udid]))
	for pid, cmd := range p.owned[udid] {
		if cur, ok := cmds[pid]; ok && cur == cmd {
			owned[pid] = cmd
		}
	}
	for _, pid := range tree {
		owned[pid] = cmds[pid]
	}
	p.owned[udid] = owned
	p.mu.Unlock()
	return nil
}

// Thaw SIGCONTs the tree cached at park time. Idempotent; a no-op (nil)
// for sims that are not parked. Exited PIDs (ESRCH) are skipped.
func (p *Parker) Thaw(udid string) error {
	p.mu.Lock()
	tree, ok := p.parked[udid]
	p.mu.Unlock()
	if !ok {
		return nil
	}
	var firstErr error
	for _, pid := range tree {
		if err := p.host.Signal(pid, syscall.SIGCONT); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("thaw %s: SIGCONT %d: %w", udid, pid, err)
		}
	}
	if firstErr != nil {
		// Keep the parked entry: the tree may still be (partly) stopped,
		// and dropping it would strand it past IsParked/ThawAll recovery.
		return firstErr
	}
	p.mu.Lock()
	delete(p.parked, udid)
	p.mu.Unlock()
	return nil
}

// ForgetParked drops a stale parked entry (the tree died outside the
// daemon) without signalling anything. The owned ledger is kept so the
// orphan reaper can still clean up any survivors.
func (p *Parker) ForgetParked(udid string) {
	p.mu.Lock()
	delete(p.parked, udid)
	p.mu.Unlock()
}

// IsParked reports whether the UDID is currently parked.
func (p *Parker) IsParked(udid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.parked[udid]
	return ok
}

// ParkedCount returns how many sims are currently parked.
func (p *Parker) ParkedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.parked)
}

// ParkedTree returns the PID tree cached at park time, or nil.
func (p *Parker) ParkedTree(udid string) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.parked[udid]...)
}

// OwnedTree returns the last tree this daemon parked for the UDID (kept
// across thaw/shutdown for the orphan reaper) as pid -> recorded command,
// or nil.
func (p *Parker) OwnedTree(udid string) map[int]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.owned[udid] == nil {
		return nil
	}
	out := make(map[int]string, len(p.owned[udid]))
	for pid, cmd := range p.owned[udid] {
		out[pid] = cmd
	}
	return out
}

// ForgetOwned drops the ownership record (after a successful reap or when
// the sim is deleted for good).
func (p *Parker) ForgetOwned(udid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.owned, udid)
}

// ThawAll resumes every parked sim (daemon shutdown path: never leave
// trees stopped behind a dead daemon).
func (p *Parker) ThawAll() {
	p.mu.Lock()
	udids := make([]string, 0, len(p.parked))
	for u := range p.parked {
		udids = append(udids, u)
	}
	p.mu.Unlock()
	for _, u := range udids {
		_ = p.Thaw(u)
	}
}
