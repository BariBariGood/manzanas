package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/BariBariGood/manzanas/internal/client"
)

// printJSON writes v as indented JSON to the command's stdout.
func (a *appEnv) printJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// emit prints v as JSON when --json is set, else calls human().
func (a *appEnv) emit(v any, human func(w io.Writer)) error {
	if a.json {
		return a.printJSON(v)
	}
	human(a.stdout)
	return nil
}

// newFlagSet builds a flag.FlagSet that reports errors instead of exiting.
// Every subcommand flag set also accepts the global --json flag so it works
// in any position on the command line.
func (a *appEnv) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.BoolVar(&a.json, "json", a.json, "machine-readable JSON output")
	fs.Func("daemon", "daemon address", func(v string) error {
		a.client = client.New(v)
		a.client.SetToken(a.token)
		return nil
	})
	fs.Func("token", "bearer token", func(v string) error {
		a.token = v
		a.client.SetToken(v)
		return nil
	})
	return fs
}

// requireArg returns args[i] or an error naming what was missing.
// Flag-looking values are rejected so a misplaced flag never becomes an ID.
func requireArg(args []string, i int, what string) (string, error) {
	if i >= len(args) {
		return "", fmt.Errorf("missing %s argument", what)
	}
	if strings.HasPrefix(args[i], "-") {
		return "", fmt.Errorf("expected %s, got flag %q (positional arguments must come before flags)", what, args[i])
	}
	return args[i], nil
}
