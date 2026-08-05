package state

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// fakeRunner simulates simctl and the CoreSimulator filesystem in memory.
type fakeRunner struct {
	mu sync.Mutex
	// states maps udid -> "Booted"|"Shutdown".
	states map[string]string
	// data maps udid -> opaque "data directory contents".
	data map[string]string
	// calls records every simctl invocation.
	calls [][]string
	// stdins records stdin passed to SimctlInput calls.
	stdins [][]byte
	// failOn maps a simctl subcommand to an error message.
	failOn map[string]string
	nextID int
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		states: map[string]string{},
		data:   map[string]string{},
		failOn: map[string]string{},
	}
}

func (f *fakeRunner) Simctl(ctx context.Context, args ...string) ([]byte, error) {
	return f.SimctlInput(ctx, nil, args...)
}

func (f *fakeRunner) SimctlInput(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	if stdin != nil {
		f.stdins = append(f.stdins, stdin)
	}
	if msg, ok := f.failOn[args[0]]; ok {
		return nil, fmt.Errorf("simctl %s: %s", args[0], msg)
	}
	switch args[0] {
	case "list":
		type dev struct {
			UDID  string `json:"udid"`
			State string `json:"state"`
		}
		var devs []dev
		for u, st := range f.states {
			devs = append(devs, dev{UDID: u, State: st})
		}
		out, _ := json.Marshal(map[string]any{"devices": map[string]any{"iOS": devs}})
		return out, nil
	case "clone":
		f.nextID++
		clone := fmt.Sprintf("CC000000-0000-4000-8000-%012d", f.nextID)
		f.states[clone] = "Shutdown"
		f.data[clone] = f.data[args[1]]
		return []byte(clone + "\n"), nil
	case "delete":
		delete(f.states, args[1])
		delete(f.data, args[1])
		return nil, nil
	case "erase":
		if f.states[args[1]] != "Shutdown" {
			return nil, fmt.Errorf("simctl erase: Unable to erase in current state: Booted")
		}
		f.data[args[1]] = "erased"
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
	}
	return nil, nil
}

func (f *fakeRunner) DeviceDataDir(udid string) string {
	return filepath.Join("/devices", udid, "data")
}

func (f *fakeRunner) ReplaceDir(ctx context.Context, src, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	srcUDID := filepath.Base(filepath.Dir(src))
	dstUDID := filepath.Base(filepath.Dir(dst))
	f.data[dstUDID] = f.data[srcUDID]
	return nil
}

func (f *fakeRunner) lastCall() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeRunner) callStrings() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}
