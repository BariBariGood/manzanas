package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/BariBariGood/manzanas/internal/mcp"
)

// cmdMCP implements `manzanas mcp`: serve MCP tools over stdio, proxying to
// the configured daemon.
func cmdMCP(ctx context.Context, app *appEnv, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("mcp: unexpected argument %q", args[0])
	}
	err := mcp.New(app.client).Serve(ctx, os.Stdin, os.Stdout)
	if errors.Is(err, context.Canceled) {
		// SIGINT/SIGTERM is the normal way an MCP host stops a stdio
		// server; treat it as a clean shutdown.
		return nil
	}
	return err
}
