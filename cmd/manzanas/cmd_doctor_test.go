package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BariBariGood/manzanas/internal/client"
	"github.com/BariBariGood/manzanas/proto"
)

func TestDoctorRendersTableAndExitsNonZeroOnFail(t *testing.T) {
	f := newFakeDaemon()
	defer f.Close()

	// All green: table + trailing verdict, nil error.
	out, _, err := runCLI(t, f, false, "doctor")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got.Method != "GET" || got.Path != "/v0/doctor" {
		t.Fatalf("wrong request: %+v", got)
	}
	if !strings.Contains(out, "daemon") || !strings.Contains(out, "all checks passed") {
		t.Fatalf("human output: %q", out)
	}

	// --target maps to the query parameter.
	if _, _, err := runCLI(t, f, false, "doctor", "--target", "UDID-1"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got.Path != "/v0/doctor" || got.Query != "target=UDID-1" {
		t.Fatalf("wrong request: %+v", got)
	}

	// --json emits the report verbatim.
	out, _, err = runCLI(t, f, true, "doctor")
	if err != nil {
		t.Fatal(err)
	}
	var rep proto.DoctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || !rep.OK {
		t.Fatalf("--json output: %v %q", err, out)
	}

	// A failing check: remedy line rendered, non-nil error (CI-friendly
	// non-zero exit).
	f.doctorFail = true
	out, _, err = runCLI(t, f, false, "doctor")
	if err == nil {
		t.Fatal("doctor with failing checks returned nil error")
	}
	if !strings.Contains(out, "fail") || !strings.Contains(out, "fix: grant Screen Recording") {
		t.Fatalf("failing output missing remedy: %q", out)
	}
}

func TestDoctorDaemonUnreachable(t *testing.T) {
	var out bytes.Buffer
	app := &appEnv{client: client.New("127.0.0.1:1"), stdout: &out, stderr: &out}
	err := cmdDoctor(context.Background(), app, nil)
	if err == nil {
		t.Fatal("unreachable daemon returned nil error")
	}
	if s := out.String(); !strings.Contains(s, "daemon unreachable") || !strings.Contains(s, "fix: start manzanasd") {
		t.Fatalf("unreachable output: %q", s)
	}
}
