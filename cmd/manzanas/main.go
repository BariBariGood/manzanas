// Command manzanas is the thin client CLI for manzanasd. Every subcommand
// maps 1:1 onto the v0 wire protocol (proto/PROTOCOL.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/BariBariGood/manzanas/internal/client"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

const usage = `manzanas — thin client for manzanasd (iOS simulator fleet daemon)

Usage: manzanas [--daemon ADDR] [--json] <command> [args]

Commands:
  targets                              list targets
  lease acquire|wait|renew|release|ls  manage leases (acquire --wait queues)
  boot|shutdown UDID                   boot/shut down a leased target
  tap|swipe|type|button                HID actions (require --lease)
  observe                              compact a11y tree
  screenshot -o FILE.png               capture the screen
  record start|stop                    screen recording (requires --lease)
  app install|launch|terminate         app lifecycle
  state snapshot|restore|fixture       deterministic state control
  stream url                           print a live view URL
  journal tail RUN_ID                  print (or --follow) the run journal
  fleet hosts|placements|hints         broker fleet views (--daemon BROKER)
  mcp                                  serve MCP tools over stdio
  version                              print the client version

Global flags:
  --daemon ADDR   daemon address (default $MANZANASD_ADDR or 127.0.0.1:7433)
  --json          machine-readable JSON output
`

// command is one CLI subcommand: it parses its own args and runs against
// the client. Registered in the commands index below.
type command func(ctx context.Context, app *appEnv, args []string) error

var commands = map[string]command{
	"targets":    cmdTargets,
	"lease":      cmdLease,
	"boot":       cmdBoot,
	"shutdown":   cmdShutdownTarget,
	"tap":        cmdTap,
	"swipe":      cmdSwipe,
	"type":       cmdType,
	"button":     cmdButton,
	"observe":    cmdObserve,
	"screenshot": cmdScreenshot,
	"app":        cmdApp,
	"state":      cmdState,
	"record":     cmdRecord,
	"stream":     cmdStream,
	"journal":    cmdJournal,
	"fleet":      cmdFleet,
	"mcp":        cmdMCP,
}

// appEnv carries the resolved global flags and IO for a command run.
type appEnv struct {
	client *client.Client
	json   bool
	stdout io.Writer
	stderr io.Writer
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "manzanas:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	addr := os.Getenv("MANZANASD_ADDR")
	jsonOut := false
	// Peel global flags off the front so subcommands own the rest.
	for len(args) > 0 {
		switch {
		case args[0] == "--json":
			jsonOut = true
			args = args[1:]
		case args[0] == "--daemon" && len(args) > 1:
			addr = args[1]
			args = args[2:]
		case len(args[0]) > 9 && args[0][:9] == "--daemon=":
			addr = args[0][9:]
			args = args[1:]
		case args[0] == "-h" || args[0] == "--help" || args[0] == "help":
			fmt.Print(usage)
			return nil
		case args[0] == "--version" || args[0] == "version":
			fmt.Printf("manzanas %s\n", version)
			return nil
		default:
			goto dispatch
		}
	}
dispatch:
	if len(args) == 0 {
		fmt.Print(usage)
		return errors.New("no command given")
	}
	cmd, ok := commands[args[0]]
	if !ok {
		return fmt.Errorf("unknown command %q (see manzanas --help)", args[0])
	}
	app := &appEnv{client: client.New(addr), json: jsonOut, stdout: os.Stdout, stderr: os.Stderr}
	return cmd(ctx, app, args[1:])
}
