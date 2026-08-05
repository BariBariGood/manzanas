package warm

import (
	"sort"
	"strings"
)

// simTree returns the PIDs of the simulator's process tree, root
// (launchd_sim) first, then descendants in BFS order. It returns nil when
// no launchd_sim for the UDID is running.
func simTree(procs []Proc, udid string) []int {
	root := findLaunchdSim(procs, udid)
	if root == 0 {
		return nil
	}
	children := make(map[int][]int, len(procs))
	for _, p := range procs {
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	tree := []int{root}
	for i := 0; i < len(tree); i++ {
		tree = append(tree, children[tree[i]]...)
	}
	return tree
}

// findLaunchdSim locates the launchd_sim process rooted at the given
// simulator UDID (its command line embeds
// .../Devices/<UDID>/data/var/run/launchd_bootstrap.plist).
func findLaunchdSim(procs []Proc, udid string) int {
	u := strings.ToUpper(udid)
	for _, p := range procs {
		if strings.Contains(p.Command, "launchd_sim") &&
			strings.Contains(strings.ToUpper(p.Command), u) {
			return p.PID
		}
	}
	return 0
}

// stoppedPIDs returns the subset of pids whose process is SIGSTOPped
// (ps state contains 'T').
func stoppedPIDs(procs []Proc, pids []int) []int {
	byPID := make(map[int]Proc, len(procs))
	for _, p := range procs {
		byPID[p.PID] = p
	}
	var stopped []int
	for _, pid := range pids {
		if p, ok := byPID[pid]; ok && strings.Contains(p.State, "T") {
			stopped = append(stopped, pid)
		}
	}
	return stopped
}

// anyAlive reports whether any of the given PIDs is in the process table.
func anyAlive(procs []Proc, pids []int) bool {
	alive := make(map[int]bool, len(procs))
	for _, p := range procs {
		alive[p.PID] = true
	}
	for _, pid := range pids {
		if alive[pid] {
			return true
		}
	}
	return false
}

// liveOwned returns the subset of recorded pids that are still alive and
// still running the same command recorded at park time, so the reaper
// only ever touches processes this daemon owns — never a recycled PID or
// another agent's tree.
func liveOwned(procs []Proc, owned map[int]string) []int {
	byPID := make(map[int]Proc, len(procs))
	for _, p := range procs {
		byPID[p.PID] = p
	}
	var live []int
	for pid, cmd := range owned {
		p, ok := byPID[pid]
		if ok && p.Command == cmd {
			live = append(live, pid)
		}
	}
	sort.Ints(live)
	return live
}

// sumRSSKB sums the RSS of the given pids as they appear in procs. It is
// NOT a phys_footprint: on macOS per-process RSS double-counts shared
// memory across a tree (use Host.TreeFootprintKB there).
func sumRSSKB(procs []Proc, pids []int) int64 {
	want := make(map[int]bool, len(pids))
	for _, pid := range pids {
		want[pid] = true
	}
	var total int64
	for _, p := range procs {
		if want[p.PID] {
			total += p.RSSKB
		}
	}
	return total
}
