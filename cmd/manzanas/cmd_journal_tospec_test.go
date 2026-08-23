package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/client"
	"github.com/BariBariGood/manzanas/internal/journal"
	"github.com/BariBariGood/manzanas/internal/runspec"
)

func TestJournalToSpec(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()
	// The fake daemon's run_1 journal: targets.boot (lifecycle, dropped),
	// a failed tap (dropped by default with everything after), and a
	// screenshot (pure read, dropped). Default export therefore has no
	// steps but still names a valid target from the meta.
	out, _, err := runCLI(t, f, false, "journal", "to-spec", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := runspec.Parse([]byte(out))
	if err != nil {
		t.Fatalf("emitted YAML rejected by strict parser: %v\n%s", err, out)
	}
	if spec.Target.UDID != "UDID-1" || len(spec.Steps) != 0 {
		t.Fatalf("spec = %+v", spec)
	}

	// --include-failed keeps the failed tap; --keep-observations keeps
	// the screenshot read.
	out, _, err = runCLI(t, f, false, "journal", "to-spec", "run_1",
		"--include-failed", "--keep-observations")
	if err != nil {
		t.Fatal(err)
	}
	if spec, err = runspec.Parse([]byte(out)); err != nil {
		t.Fatal(err)
	}
	if len(spec.Steps) != 2 || spec.Steps[0].Action != "tap" || spec.Steps[1].Action != "screenshot" {
		t.Fatalf("steps = %+v", spec.Steps)
	}

	// -o writes to a file instead of stdout.
	path := filepath.Join(t.TempDir(), "spec.yaml")
	out, _, err = runCLI(t, f, false, "journal", "to-spec", "run_1", "-o", path)
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("-o still wrote to stdout: %q", out)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runspec.Parse(b); err != nil {
		t.Fatalf("-o file content invalid: %v\n%s", err, b)
	}

	if _, _, err := runCLI(t, f, false, "journal", "to-spec", "run_1", "extra"); err == nil {
		t.Fatal("expected error for unexpected argument")
	}
}

func TestJournalToSpecOffline(t *testing.T) {
	// --journal-dir reads straight from the on-disk store, like
	// journal export.
	dir := t.TempDir()
	store, err := journal.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	runID := "lse_spec_off"
	if err := store.WriteMeta(runID, journal.RunMeta{
		FormatVersion: journal.FormatVersion, RunID: runID, Purpose: "offline flow",
		Runtime: "iOS 26.0", DeviceType: "iPhone 17 Pro",
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, p := range []map[string]any{
		{"action": "leases.acquire", "status": "ok"},
		{"action": "type_into_element", "status": "ok",
			"params": map[string]any{"id": "username", "text": "agent"}},
		{"action": "observe", "status": "ok"},
		{"action": "tap_element", "status": "ok",
			"params": map[string]any{"label": "Sign In"}},
	} {
		if _, err := store.Append(ctx, runID, "action", p); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	app := &appEnv{client: client.New("127.0.0.1:1"), stdout: &out, stderr: &out}
	if err := cmdJournal(ctx, app, []string{"to-spec", runID, "--journal-dir", dir}); err != nil {
		t.Fatal(err)
	}
	spec, err := runspec.Parse(out.Bytes())
	if err != nil {
		t.Fatalf("emitted YAML rejected: %v\n%s", err, out.String())
	}
	if spec.Name != "offline flow" || spec.Target.Runtime != "iOS 26.0" {
		t.Fatalf("spec meta wrong: %+v", spec)
	}
	if len(spec.Steps) != 2 || spec.Steps[0].Action != "type_into_element" || spec.Steps[1].Action != "tap_element" {
		t.Fatalf("steps = %+v", spec.Steps)
	}
	if !strings.Contains(out.String(), "text: agent") {
		t.Fatalf("params missing from YAML:\n%s", out.String())
	}

	// Unknown run in the offline store is a clear error.
	if err := cmdJournal(ctx, app, []string{"to-spec", "nope", "--journal-dir", dir}); err == nil {
		t.Fatal("expected error for unknown offline run")
	}
}
