package state

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// fsFakeRunner simulates simctl against a real filesystem rooted at base,
// so image pack/unpack and ReplaceDir run against actual directories
// (Linux-safe). Complements the in-memory fakeRunner used by engine tests.
type fsFakeRunner struct {
	mu     sync.Mutex
	base   string
	states map[string]string // udid -> "Booted"|"Shutdown"
	names  map[string]string // udid -> device name
	calls  [][]string
	failOn map[string]string
	nextID int
	// disabled maps udid -> launchctl-disabled services. Like the real
	// thing it is keyed to the UDID outside the data directory: create
	// starts empty, ReplaceDir never copies it, and erase wipes it.
	disabled map[string]map[string]bool
	// daemons are the running services every fresh sim starts with.
	daemons []string
	// onReplace, when set, runs after every successful ReplaceDir — used
	// to simulate another agent booting a hidden sim mid-stamp.
	onReplace func(dst string)
}

func newFSFakeRunner(base string) *fsFakeRunner {
	return &fsFakeRunner{
		base:     base,
		states:   map[string]string{},
		names:    map[string]string{},
		failOn:   map[string]string{},
		disabled: map[string]map[string]bool{},
		daemons:  []string{"com.apple.apsd", "com.apple.diagnosticd", "com.apple.locationd"},
	}
}

func (f *fsFakeRunner) Simctl(ctx context.Context, args ...string) ([]byte, error) {
	return f.SimctlInput(ctx, nil, args...)
}

func (f *fsFakeRunner) SimctlInput(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	if msg, ok := f.failOn[args[0]]; ok {
		return nil, fmt.Errorf("simctl %s: %s", args[0], msg)
	}
	switch args[0] {
	case "list":
		if args[1] == "runtimes" {
			out, _ := json.Marshal(map[string]any{"runtimes": []map[string]any{
				{"name": "iOS 26.5", "identifier": "com.apple.CoreSimulator.SimRuntime.iOS-26-5", "isAvailable": true},
				{"name": "iOS 17.0", "identifier": "com.apple.CoreSimulator.SimRuntime.iOS-17-0", "isAvailable": false},
			}})
			return out, nil
		}
		type dev struct {
			UDID  string `json:"udid"`
			State string `json:"state"`
			Name  string `json:"name"`
		}
		var devs []dev
		for u, st := range f.states {
			devs = append(devs, dev{UDID: u, State: st, Name: f.names[u]})
		}
		out, _ := json.Marshal(map[string]any{"devices": map[string]any{"iOS": devs}})
		return out, nil
	case "create":
		f.nextID++
		udid := fmt.Sprintf("AA000000-0000-4000-8000-%012d", f.nextID)
		f.states[udid] = "Shutdown"
		f.names[udid] = args[1]
		dir := filepath.Join(f.base, udid)
		if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "data", "fresh.txt"), []byte("factory"), 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "device.plist"), []byte("<plist>"+udid+"</plist>"), 0o644); err != nil {
			return nil, err
		}
		return []byte(udid + "\n"), nil
	case "delete":
		delete(f.states, args[1])
		delete(f.names, args[1])
		return nil, os.RemoveAll(filepath.Join(f.base, args[1]))
	case "rename":
		if _, ok := f.states[args[1]]; !ok {
			return nil, fmt.Errorf("simctl rename: device not found: %s", args[1])
		}
		f.names[args[1]] = args[2]
		return nil, nil
	case "boot":
		f.states[args[1]] = "Booted"
		return nil, nil
	case "shutdown":
		if f.states[args[1]] == "Shutdown" {
			return nil, fmt.Errorf("simctl shutdown: Unable to shutdown device in current state: Shutdown")
		}
		f.states[args[1]] = "Shutdown"
		return nil, nil
	case "erase":
		if f.states[args[1]] != "Shutdown" {
			return nil, fmt.Errorf("simctl erase: Unable to erase in current state: %s", f.states[args[1]])
		}
		delete(f.disabled, args[1]) // erase wipes the per-UDID launchctl config
		return nil, nil
	case "spawn":
		return f.spawnLaunchctl(args)
	}
	return nil, nil
}

// spawnLaunchctl handles `spawn <udid> launchctl ...`.
func (f *fsFakeRunner) spawnLaunchctl(args []string) ([]byte, error) {
	udid := args[1]
	if f.states[udid] != "Booted" {
		return nil, fmt.Errorf("simctl spawn: Unable to spawn in current state: %s", f.states[udid])
	}
	if len(args) < 3 || args[2] != "launchctl" {
		return nil, fmt.Errorf("simctl spawn: unsupported: %v", args[2:])
	}
	switch args[3] {
	case "print-disabled":
		var b strings.Builder
		b.WriteString("disabled services = {\n")
		for svc := range f.disabled[udid] {
			fmt.Fprintf(&b, "\t\"%s\" => disabled\n", svc)
		}
		b.WriteString("}\n")
		return []byte(b.String()), nil
	case "disable":
		svc := strings.TrimPrefix(args[4], "system/")
		if f.disabled[udid] == nil {
			f.disabled[udid] = map[string]bool{}
		}
		f.disabled[udid][svc] = true
		return nil, nil
	case "list":
		var b strings.Builder
		b.WriteString("PID\tStatus\tLabel\n")
		pid := 100
		for _, svc := range f.daemons {
			if f.disabled[udid][svc] {
				continue
			}
			fmt.Fprintf(&b, "%d\t0\t%s\n", pid, svc)
			pid++
		}
		return []byte(b.String()), nil
	}
	return nil, fmt.Errorf("simctl spawn launchctl: unsupported: %s", args[3])
}

// disableAll marks every default daemon disabled on udid (what a
// successful simslim run does), for use inside a test SlimFunc.
func (f *fsFakeRunner) disableAll(udid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	set := map[string]bool{}
	for _, svc := range f.daemons {
		set[svc] = true
	}
	f.disabled[udid] = set
}

// disabledOn returns the disabled services recorded for udid.
func (f *fsFakeRunner) disabledOn(udid string) map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for svc := range f.disabled[udid] {
		out[svc] = true
	}
	return out
}

func (f *fsFakeRunner) DeviceDataDir(udid string) string {
	return filepath.Join(f.base, udid, "data")
}

func (f *fsFakeRunner) ReplaceDir(ctx context.Context, src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := copyTree(src, dst); err != nil {
		return err
	}
	if f.onReplace != nil {
		f.onReplace(dst)
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			return os.MkdirAll(dest, fi.Mode())
		case fi.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, dest)
		default:
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		}
	})
}

// udidsWithPrefix returns UDIDs of devices whose name has the prefix.
func (f *fsFakeRunner) udidsWithPrefix(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for u, n := range f.names {
		if strings.HasPrefix(n, prefix) {
			out = append(out, u)
		}
	}
	return out
}
