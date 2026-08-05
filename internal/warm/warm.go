// Package warm implements the park/thaw warm pool: pool simulators are
// booted, slimmed, then PARKED (their whole process tree SIGSTOPped from
// launchd_sim down) so they cost ~0 CPU while idle. On lease grant the
// tree is SIGCONTed (thawed) in ~25ms on Apple Silicon / ~225ms on Intel
// (using the PID list cached at park time; a naive ps-walk at thaw time
// costs 0.4-2s on M3 and ~6.5s on Intel). On release+reset the sim is
// erased, re-booted, and parked again.
//
// The package also owns the host safety guards: per-host capacity classes
// (max running / parked / concurrent boots), a load gate, a free-disk
// gate, a footprint watchdog for runaway sims, and an orphan reaper for
// leftover launchd_sim trees.
//
// Invariants callers must respect:
//   - ALWAYS thaw before shutdown: `simctl shutdown` on a parked sim
//     wedges for ~34s. Use Pool.SafeShutdown or the Guard registry.
//   - While parked, AXe fails and `simctl spawn` hangs; do not route
//     actions at a parked sim (the pool thaws on lease grant).
//   - Double STOP / double CONT are harmless; Park/Thaw are idempotent.
package warm

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Proc is one row of the host process table.
type Proc struct {
	PID     int
	PPID    int
	RSSKB   int64  // resident set size in KiB (approximates phys_footprint)
	State   string // ps state string; contains 'T' when SIGSTOPped
	Command string
}

// Host abstracts the process table, signals, and host gauges so the pool
// is testable on Linux with a fake.
type Host interface {
	// Processes returns the full process table (pid, ppid, rss, command).
	Processes(ctx context.Context) ([]Proc, error)
	// Signal sends sig to pid. Must return nil for ESRCH (already gone).
	Signal(pid int, sig syscall.Signal) error
	// LoadAvg1 returns the 1-minute load average.
	LoadAvg1() (float64, error)
	// NumCPU returns the logical core count.
	NumCPU() int
	// FreeDiskBytes returns free bytes on the volume containing path.
	FreeDiskBytes(path string) (uint64, error)
	// TreeFootprintKB returns the combined physical memory footprint
	// (KiB) of the given process tree. On macOS this must be the kernel
	// phys_footprint: summing per-process RSS over a sim tree overcounts
	// ~8x (each of the ~90+ processes double-counts the shared dyld
	// cache), so a healthy ~1 GB slim sim reads as 6-12 GiB.
	TreeFootprintKB(ctx context.Context, pids []int) (int64, error)
}

// runSimctl runs `xcrun simctl <args...>`, folding stderr into the error.
func runSimctl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "xcrun", append([]string{"simctl"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("simctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// osHost is the real Host backed by ps/sysctl/df.
type osHost struct{}

// NewOSHost returns the production Host implementation.
func NewOSHost() Host { return osHost{} }

func (osHost) Processes(ctx context.Context) ([]Proc, error) {
	// -ww: never truncate the command, the sim UDID must survive.
	out, err := exec.CommandContext(ctx, "ps", "-axww", "-o", "pid=,ppid=,rss=,state=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return parsePS(out)
}

func parsePS(out []byte) ([]Proc, error) {
	var procs []Proc
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		rss, err3 := strconv.ParseInt(fields[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		procs = append(procs, Proc{PID: pid, PPID: ppid, RSSKB: rss, State: fields[3], Command: strings.Join(fields[4:], " ")})
	}
	return procs, sc.Err()
}

func (osHost) Signal(pid int, sig syscall.Signal) error {
	err := syscall.Kill(pid, sig)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

func (osHost) LoadAvg1() (float64, error) {
	if runtime.GOOS == "darwin" {
		// `sysctl -n vm.loadavg` prints "{ 1.23 4.56 7.89 }".
		out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
		if err != nil {
			return 0, fmt.Errorf("sysctl vm.loadavg: %w", err)
		}
		fields := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
		if len(fields) < 1 {
			return 0, fmt.Errorf("sysctl vm.loadavg: unparseable %q", out)
		}
		return strconv.ParseFloat(fields[0], 64)
	}
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("/proc/loadavg: unparseable")
	}
	return strconv.ParseFloat(fields[0], 64)
}

func (osHost) NumCPU() int { return runtime.NumCPU() }

// TreeFootprintKB shells out to /usr/bin/footprint on macOS (per-pid
// task_info phys_footprint, no root needed for same-user sims) and sums
// the per-process footprints. Elsewhere it falls back to summed ps RSS.
func (h osHost) TreeFootprintKB(ctx context.Context, pids []int) (int64, error) {
	if len(pids) == 0 {
		return 0, nil
	}
	if runtime.GOOS != "darwin" {
		procs, err := h.Processes(ctx)
		if err != nil {
			return 0, err
		}
		return sumRSSKB(procs, pids), nil
	}
	args := make([]string, 0, 2*len(pids)+3)
	for _, pid := range pids {
		args = append(args, "--pid", strconv.Itoa(pid))
	}
	args = append(args, "--noCategories", "-f", "bytes")
	// footprint exits non-zero / prints "Unable to analyze" for pids that
	// exited between the tree walk and now; the survivors still print, so
	// only a fully unparseable output is an error.
	out, _ := exec.CommandContext(ctx, "/usr/bin/footprint", args...).CombinedOutput()
	kb, n := parseFootprint(out)
	if n == 0 {
		return 0, fmt.Errorf("footprint: no phys_footprint parsed: %s", firstLine(out))
	}
	return kb, nil
}

// parseFootprint sums the "phys_footprint: <N> B" lines of
// `footprint --noCategories -f bytes` output, returning KiB and how many
// processes were parsed.
func parseFootprint(out []byte) (kb int64, n int) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		rest, ok := strings.CutPrefix(line, "phys_footprint:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 1 {
			continue
		}
		b, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		kb += b / 1024
		n++
	}
	return kb, n
}

func firstLine(out []byte) string {
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func (osHost) FreeDiskBytes(path string) (uint64, error) {
	// `df -Pk` is portable across macOS and Linux; column 4 is available KiB.
	out, err := exec.Command("df", "-Pk", path).Output()
	if err != nil {
		return 0, fmt.Errorf("df %s: %w", path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("df %s: unparseable output", path)
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, fmt.Errorf("df %s: unparseable row", path)
	}
	kb, err := strconv.ParseUint(fields[3], 10, 64)
	if err != nil {
		return 0, err
	}
	return kb * 1024, nil
}
