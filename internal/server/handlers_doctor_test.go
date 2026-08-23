package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BariBariGood/manzanas/internal/doctor"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/proto"
)

// TestDoctorEndpoint serves a real doctor.Env shaped like `manzanasd
// --mock` (no sim tooling, no pool, no device manager) through
// GET /v0/doctor: the report is OK with hardware checks n/a — the
// mock-daemon e2e shape.
func TestDoctorEndpoint(t *testing.T) {
	srv := New(registry.NewMock(), nil, nil)
	env := doctor.Env{Build: "test", Protocol: proto.Version,
		SimTooling: false, JournalDir: t.TempDir()}
	srv.SetDoctor(env.Report)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var r proto.DoctorReport
	resp := doJSON(t, "GET", ts.URL+"/v0/doctor", nil, &r)
	if resp.StatusCode != http.StatusOK || !r.OK {
		t.Fatalf("doctor: %d %+v", resp.StatusCode, r)
	}
	byName := map[string]proto.DoctorCheck{}
	for _, c := range r.Checks {
		byName[c.Name] = c
	}
	if byName["daemon"].Status != proto.DoctorOK {
		t.Fatalf("daemon check = %+v", byName["daemon"])
	}
	for _, name := range []string{"xcrun", "ios_runtime", "devicectl", "boot_gates", "devices"} {
		if byName[name].Status != proto.DoctorNA {
			t.Fatalf("%s = %+v, want n/a on a mock daemon", name, byName[name])
		}
	}
}

// TestDoctorEndpointTargetAndUnwired covers the ?target passthrough and
// the not_implemented answer when no doctor is wired.
func TestDoctorEndpointTargetAndUnwired(t *testing.T) {
	srv := New(registry.NewMock(), nil, nil)
	var gotTarget string
	srv.SetDoctor(func(_ context.Context, target string) proto.DoctorReport {
		gotTarget = target
		return proto.DoctorReport{OK: true}
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	var r proto.DoctorReport
	if resp := doJSON(t, "GET", ts.URL+"/v0/doctor?target=UDID-9", nil, &r); resp.StatusCode != http.StatusOK {
		t.Fatalf("doctor with target: %d", resp.StatusCode)
	}
	if gotTarget != "UDID-9" {
		t.Fatalf("target = %q, want UDID-9", gotTarget)
	}

	bare := New(registry.NewMock(), nil, nil)
	ts2 := httptest.NewServer(bare.Handler())
	t.Cleanup(ts2.Close)
	var we struct {
		Code string `json:"code"`
	}
	if resp := doJSON(t, "GET", ts2.URL+"/v0/doctor", nil, &we); resp.StatusCode != http.StatusNotImplemented ||
		we.Code != proto.ErrNotImplemented {
		t.Fatalf("unwired doctor: %d %+v, want 501 not_implemented", resp.StatusCode, we)
	}
}
