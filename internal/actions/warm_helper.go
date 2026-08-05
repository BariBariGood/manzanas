package actions

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Helper is one warm, resident connection to a single simulator: it
// services repeated actions without re-bootstrapping FBSimulatorControl.
// Implementations must be safe to Close concurrently with Call.
type Helper interface {
	// Call executes one operation. Action-level failures (the helper is
	// healthy but the op failed) return *Error; transport failures (the
	// helper died or is unreachable) return *TransportError so the pool
	// can restart it.
	Call(ctx context.Context, op string, args map[string]any) (map[string]any, error)
	// Close terminates the helper and releases its resources.
	Close() error
}

// HelperFactory spawns a warm helper bound to one target UDID.
type HelperFactory func(udid string) (Helper, error)

// TransportError marks a helper as broken (crashed, hung, or unreachable),
// as opposed to a healthy helper reporting an action failure.
type TransportError struct {
	Err error
	// Delivered is true when the request had already been written to the
	// helper before the failure, so the simulator may have performed the
	// input. Non-idempotent ops (tap, key, ...) must not be replayed or
	// re-run cold in that case.
	Delivered bool
}

func (e *TransportError) Error() string { return "simbridge transport: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

func transportErr(format string, a ...any) *TransportError {
	return &TransportError{Err: fmt.Errorf(format, a...)}
}

// bridgeRequest / bridgeResponse are the simbridge JSON-lines wire shapes.
// The schema is owned by helpers/simbridge; see docs/actions-warm.md.
type bridgeRequest struct {
	ID   string         `json:"id"`
	Op   string         `json:"op"`
	Args map[string]any `json:"args,omitempty"`
}

type bridgeResponse struct {
	ID     string         `json:"id,omitempty"`
	Event  string         `json:"event,omitempty"`
	OK     bool           `json:"ok"`
	Code   string         `json:"code,omitempty"`
	Error  string         `json:"error,omitempty"`
	Result map[string]any `json:"result,omitempty"`
}

// procHelper drives one resident simbridge process over stdin/stdout
// JSON lines. Calls are serialized: the helper handles one op at a time.
type procHelper struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Reader
	nextID int
	closed bool
}

// startupTimeout bounds how long simbridge may take to load frameworks and
// connect to the simulator before it is declared unusable.
const startupTimeout = 30 * time.Second

// NewProcHelper spawns `simbridge --udid <udid>` and waits for its ready
// handshake. It is the production HelperFactory.
func NewProcHelper(binPath, udid string) (Helper, error) {
	cmd := exec.Command(binPath, "--udid", udid)
	// Leave Stderr nil (child gets /dev/null): an io.Writer would make
	// os/exec spawn a copy goroutine that cmd.Wait must join, which can
	// block Close indefinitely if the child leaks the pipe.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, transportErr("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, transportErr("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, transportErr("start %s: %v", binPath, err)
	}
	h := &procHelper{cmd: cmd, stdin: stdin, out: bufio.NewReaderSize(stdout, 1<<20)}
	if err := h.awaitReady(); err != nil {
		_ = h.Close()
		return nil, err
	}
	return h, nil
}

// awaitReady reads the handshake line the helper emits once frameworks are
// loaded and the simulator connection is established.
func (h *procHelper) awaitReady() error {
	type readResult struct {
		resp bridgeResponse
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		var resp bridgeResponse
		err := h.readLine(&resp)
		ch <- readResult{resp, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return transportErr("simbridge handshake: %v", r.err)
		}
		if r.resp.Event != "ready" {
			if r.resp.Error != "" {
				return transportErr("simbridge startup failed: %s", r.resp.Error)
			}
			return transportErr("simbridge handshake: unexpected first line")
		}
		return nil
	case <-time.After(startupTimeout):
		return transportErr("simbridge did not become ready within %s", startupTimeout)
	}
}

func (h *procHelper) readLine(resp *bridgeResponse) error {
	line, err := h.out.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, resp)
}

func (h *procHelper) Call(ctx context.Context, op string, args map[string]any) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, transportErr("helper is closed")
	}
	h.nextID++
	req := bridgeRequest{ID: fmt.Sprintf("%d", h.nextID), Op: op, Args: args}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, internal("encode simbridge request: %v", err)
	}
	payload = append(payload, '\n')

	type callResult struct {
		resp  bridgeResponse
		wrote bool
		err   error
	}
	ch := make(chan callResult, 1)
	go func() {
		if _, werr := h.stdin.Write(payload); werr != nil {
			ch <- callResult{err: werr}
			return
		}
		var resp bridgeResponse
		rerr := h.readLine(&resp)
		ch <- callResult{resp: resp, wrote: true, err: rerr}
	}()

	select {
	case <-ctx.Done():
		// The request may already be executing on the simulator; mark it
		// Delivered so non-idempotent ops are not replayed.
		return nil, &TransportError{Err: fmt.Errorf("call cancelled: %v", ctx.Err()), Delivered: true}
	case r := <-ch:
		if r.err != nil {
			return nil, &TransportError{Err: fmt.Errorf("simbridge io: %v", r.err), Delivered: r.wrote}
		}
		if r.resp.ID != req.ID {
			// An unsolicited line would shift the strictly-ordered stream
			// by one forever; kill the helper rather than return another
			// request's result.
			return nil, &TransportError{Err: fmt.Errorf("simbridge reply id %q does not match request id %q", r.resp.ID, req.ID), Delivered: true}
		}
		if !r.resp.OK {
			code := r.resp.Code
			if code == "" {
				code = "internal"
			}
			return nil, &Error{Code: code, Message: r.resp.Error}
		}
		return r.resp.Result, nil
	}
}

func (h *procHelper) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	h.mu.Unlock()
	_ = h.stdin.Close()
	done := make(chan struct{})
	go func() { _ = h.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
	}
	return nil
}

// DetectSimbridge looks for the simbridge helper binary at an explicit
// path, then ~/bin/simbridge, then $PATH. It returns "" when unavailable.
func DetectSimbridge(explicit string) string {
	if explicit != "" {
		if st, err := os.Stat(explicit); err == nil && !st.IsDir() {
			return explicit
		}
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, "bin", "simbridge")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("simbridge"); err == nil {
		return p
	}
	return ""
}
