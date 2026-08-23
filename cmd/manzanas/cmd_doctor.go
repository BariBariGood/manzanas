package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/BariBariGood/manzanas/internal/client"
	"github.com/BariBariGood/manzanas/proto"
)

// cmdDoctor diagnoses a host's setup: `manzanas doctor [--target UDID]`.
// The daemon aggregates the checks (GET /v0/doctor); when the daemon
// itself is unreachable the CLI degrades to a local report whose one
// failing check says how to start it, so `doctor` is the command to run
// on a machine with nothing configured at all.
func cmdDoctor(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("doctor")
	target := fs.String("target", "", "restrict per-device checks (WDA, mirror) to this UDID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := app.client.Doctor(ctx, *target)
	if err != nil {
		report = unreachableReport(app.client.Addr(), err)
	}
	if err := app.emit(report, func(w io.Writer) { renderDoctor(w, report) }); err != nil {
		return err
	}
	if !report.OK {
		return errors.New("doctor found failing checks")
	}
	return nil
}

// unreachableReport turns a failed doctor fetch into a report of its
// own, so the output shape (and the non-zero exit) is the same whether
// the daemon is broken or absent.
func unreachableReport(addr string, err error) proto.DoctorReport {
	detail := "daemon unreachable at " + addr + ": " + err.Error()
	remedy := "start manzanasd (see docs/quickstart.md; `manzanasd --mock` runs anywhere for a smoke test), or point --daemon/$MANZANASD_ADDR at a running one"
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		detail = "daemon at " + addr + " answered " + apiErr.Error()
		switch apiErr.Status {
		case http.StatusNotFound, http.StatusNotImplemented:
			detail = "daemon at " + addr + " predates the doctor endpoint"
			remedy = "upgrade manzanasd to a build serving GET /v0/doctor"
		case http.StatusUnauthorized:
			remedy = "pass the daemon's bearer token (--token / $MANZANAS_TOKEN)"
		}
	}
	return proto.DoctorReport{OK: false, Checks: []proto.DoctorCheck{{
		Name: "daemon", Status: proto.DoctorFail, Detail: detail, Remedy: remedy,
	}}}
}

// renderDoctor prints the human-readable table: one aligned line per
// check, with the remedy indented under any check that needs one.
func renderDoctor(w io.Writer, r proto.DoctorReport) {
	nameW := 0
	for _, c := range r.Checks {
		if len(c.Name) > nameW {
			nameW = len(c.Name)
		}
	}
	for _, c := range r.Checks {
		fmt.Fprintf(w, "%-5s %-*s %s\n", c.Status, nameW, c.Name, c.Detail)
		if c.Remedy != "" {
			fmt.Fprintf(w, "%-5s %-*s fix: %s\n", "", nameW, "", c.Remedy)
		}
	}
	if r.OK {
		fmt.Fprintln(w, "\nall checks passed")
	} else {
		fmt.Fprintln(w, "\nsome checks FAILED — apply the fixes above and re-run")
	}
}
