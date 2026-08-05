// Package mcp is a minimal MCP (Model Context Protocol) stdio server
// exposing lease-scoped manzanasd operations as agent tools.
//
// It implements the subset of MCP needed for tool serving — initialize,
// tools/list, tools/call over JSON-RPC 2.0 with newline-delimited framing —
// without external dependencies (the official go-sdk requires Go >= 1.23;
// this module targets 1.22).
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/BariBariGood/manzanas/internal/client"
)

const protocolVersion = "2025-06-18"

// Server serves MCP over a stdio-style transport.
type Server struct {
	client *client.Client
	tools  []Tool

	mu    sync.Mutex
	out   *json.Encoder
	owned map[string]bool // lease IDs acquired through this session
	wg    sync.WaitGroup  // in-flight tool calls
}

// New builds a Server exposing the standard manzanas toolset over c.
func New(c *client.Client) *Server {
	s := &Server{client: c, owned: make(map[string]bool)}
	s.tools = allTools()
	return s
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads newline-delimited JSON-RPC requests from r and writes
// responses to w until EOF or ctx is done. On exit it releases any leases
// acquired through this session (best-effort) so targets aren't orphaned.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	s.out = json.NewEncoder(w)
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		// Give in-flight tool calls a short grace period to finish, then
		// cancel them (e.g. an unbounded lease-queue wait after the client
		// disconnected) so the session always exits and releases leases.
		done := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cancel()
			<-done
		}
		cancel()
		s.releaseOwned()
	}()
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 0, 1<<20), 16<<20)
	// Read in a goroutine so ctx cancellation (e.g. SIGINT) still runs the
	// deferred lease release instead of blocking on stdin forever.
	lines := make(chan []byte)
	errc := make(chan error, 1)
	go func() {
		defer close(lines)
		for scan.Scan() {
			line := append([]byte(nil), scan.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		errc <- scan.Err()
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				select {
				case err := <-errc:
					return err
				default:
					return nil
				}
			}
			if len(line) == 0 {
				continue
			}
			var req rpcRequest
			if err := json.Unmarshal(line, &req); err != nil {
				s.reply(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
					Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()}})
				continue
			}
			s.dispatch(ctx, req)
		}
	}
}

func (s *Server) reply(resp rpcResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.out.Encode(resp)
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) {
	// Notifications (no ID) get no response.
	switch req.Method {
	case "initialize":
		// Echo the client's requested protocol version when it's a known
		// revision (the tool-serving subset is identical across them);
		// otherwise answer with our preferred version.
		version := protocolVersion
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &params) == nil {
			switch params.ProtocolVersion {
			case "2024-11-05", "2025-03-26", "2025-06-18":
				version = params.ProtocolVersion
			}
		}
		s.reply(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "manzanas", "version": "0.1.0"},
		}})
	case "notifications/initialized", "initialized":
		// no-op
	case "ping":
		s.reply(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})
	case "tools/list":
		defs := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			defs = append(defs, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		s.reply(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": defs}})
	case "tools/call":
		// Tool calls run concurrently so a long call (e.g. waiting in the
		// lease queue) doesn't block pings or other requests. Replies are
		// serialized by s.mu in reply.
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleToolCall(ctx, req)
		}()
	default:
		if req.ID != nil {
			s.reply(rpcResponse{JSONRPC: "2.0", ID: req.ID,
				Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}})
		}
	}
}

func (s *Server) handleToolCall(ctx context.Context, req rpcRequest) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.reply(rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}})
		return
	}
	var tool *Tool
	for i := range s.tools {
		if s.tools[i].Name == params.Name {
			tool = &s.tools[i]
			break
		}
	}
	if tool == nil {
		s.reply(rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32602, Message: "unknown tool: " + params.Name}})
		return
	}
	content, err := tool.Call(ctx, s, params.Arguments)
	if err != nil {
		// Tool errors are results with isError, per the MCP spec.
		s.reply(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		}})
		return
	}
	s.reply(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"content": content}})
}

func (s *Server) trackLease(id string)  { s.mu.Lock(); s.owned[id] = true; s.mu.Unlock() }
func (s *Server) forgetLease(id string) { s.mu.Lock(); delete(s.owned, id); s.mu.Unlock() }

func (s *Server) releaseOwned() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.owned))
	for id := range s.owned {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = s.client.ReleaseLease(rctx, id)
		cancel()
	}
}

// textContent wraps s as MCP text content.
func textContent(s string) []map[string]any {
	return []map[string]any{{"type": "text", "text": s}}
}

// jsonContent marshals v as compact JSON text content.
func jsonContent(v any) ([]map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal result: %w", err)
	}
	return textContent(string(b)), nil
}
