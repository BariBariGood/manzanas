package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/BariBariGood/manzanas/internal/mcp"
)

// mcpHealthTimeout bounds the startup daemon reachability probe.
const mcpHealthTimeout = 5 * time.Second

// cmdMCP implements `manzanas mcp`: serve MCP tools over stdio, proxying to
// the configured daemon.
func cmdMCP(ctx context.Context, app *appEnv, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("mcp: unexpected argument %q (the daemon address is a global flag: manzanas --daemon ADDR mcp, or set MANZANASD_ADDR)", args[0])
	}
	// Fail fast on a misconfigured or down daemon so the MCP host surfaces
	// one clear startup error instead of every tool call failing later.
	hctx, cancel := context.WithTimeout(ctx, mcpHealthTimeout)
	err := app.client.Health(hctx)
	cancel()
	if err != nil {
		return fmt.Errorf("mcp: daemon health check failed: %w\nStart manzanasd on the target Mac, or point this client at it with `manzanas --daemon HOST:7433 mcp` or the MANZANASD_ADDR environment variable (current: %s)", err, app.client.Addr())
	}
	fmt.Fprintf(app.stderr, "manzanas mcp: connected to manzanasd at %s; serving tools on stdio\n", app.client.Addr())
	return serveMCP(ctx, app, os.Stdin, os.Stdout)
}

// serveMCP runs the MCP session on the given streams (split out for tests).
func serveMCP(ctx context.Context, app *appEnv, in io.Reader, out io.Writer) error {
	err := mcp.New(app.client, version).Serve(ctx, in, out)
	if errors.Is(err, context.Canceled) {
		// SIGINT/SIGTERM is the normal way an MCP host stops a stdio
		// server; treat it as a clean shutdown.
		return nil
	}
	return err
}
