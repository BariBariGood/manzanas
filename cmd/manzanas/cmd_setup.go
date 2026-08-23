package main

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/BariBariGood/manzanas/internal/setup"
)

// cmdSetup runs the guided host-setup wizard: `manzanas setup`. Unlike
// the other subcommands it talks to the host, not the daemon — it
// installs/updates the GUI helpers at their stable paths, probes which
// TCC grants are missing, and walks the operator through granting them
// (never prompting without confirmation). Idempotent; re-run any time.
func cmdSetup(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("setup")
	report := fs.Bool("report", false, "detect and print state only: no installs, no Settings windows (the live probe may still trigger macOS's one-time permission dialog on a never-asked host)")
	yes := fs.Bool("yes", false, "answer yes to every confirmation")
	helpersSrc := fs.String("helpers-src", "", "repo checkout containing helpers/ (auto-detected)")
	socket := fs.String("socket", "", "mirrord unix socket (default ~/.manzanasd/mirrord.sock)")
	identity := fs.String("sign-identity", "", `codesign identity for mirrord (default: best keychain identity — REQUIRED so TCC grants survive rebuilds; "-" forces ad-hoc)`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	env := setup.New(app.stdout, app.stderr, os.Stdin)
	if app.json {
		// Human narration goes to stderr so stdout stays pure JSON.
		env.Stdout = app.stderr
	}
	res, err := env.Run(ctx, setup.Options{
		Report:       *report,
		Yes:          *yes,
		HelpersSrc:   *helpersSrc,
		Socket:       *socket,
		SignIdentity: *identity,
	})
	if err != nil {
		return err
	}
	if err := app.emit(res, func(io.Writer) {}); err != nil {
		return err
	}
	if !res.OK {
		for _, g := range res.Grants {
			if g.Status == "missing" {
				return errors.New("setup found missing grants — follow the instructions above and re-run")
			}
		}
		return errors.New("setup could not verify every grant — fix the conditions above and re-run")
	}
	return nil
}
