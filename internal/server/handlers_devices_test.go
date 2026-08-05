package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BariBariGood/manzanas/internal/lease"
	"github.com/BariBariGood/manzanas/internal/registry"
	"github.com/BariBariGood/manzanas/internal/state"
	"github.com/BariBariGood/manzanas/proto"
)

// newDeviceTestServer wires a mock fleet containing one simulator and one
// physical device, with a state engine so reset specs pass the
// resetSpecSupported gate.
func newDeviceTestServer(t *testing.T, eng state.Engine) *httptest.Server {
	t.Helper()
	reg := registry.NewMock(
		proto.Target{UDID: "SIM-1", Kind: proto.TargetSimulator, Name: "iPhone 17 Pro",
			Runtime: "iOS 26.5", DeviceType: "iPhone 17 Pro", State: proto.StateShutdown},
		proto.Target{UDID: "DEV-1", Kind: proto.TargetDevice, Name: "phone",
			Runtime: "iOS 26.5", DeviceType: "iPhone 15 Pro", State: proto.StateUnknown,
			Labels: []string{"device", "iphone-15-pro"}},
	)
	srv := New(reg, nil, nil)
	srv.SetState(eng)
	srv.SetDevicesEnabled(true)
	leases := lease.New(reg, srv.EventSink())
	leases.SetResetFunc(srv.ResetSink())
	t.Cleanup(leases.Close)
	srv.SetLeases(leases)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestAcquireRejectsResetOnDeviceLabel(t *testing.T) {
	ts := newDeviceTestServer(t, &fakeEngine{})
	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"device"}, AgentID: "a1", Reset: "erase"}, &e)
	if resp.StatusCode != http.StatusBadRequest || e.Code != proto.ErrBadRequest {
		t.Fatalf("got %d %+v", resp.StatusCode, e)
	}
}

func TestAcquireRejectsResetOnPinnedDevice(t *testing.T) {
	ts := newDeviceTestServer(t, &fakeEngine{})
	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{UDID: "DEV-1", AgentID: "a1", Reset: "erase"}, &e)
	if resp.StatusCode != http.StatusBadRequest || e.Code != proto.ErrBadRequest {
		t.Fatalf("got %d %+v", resp.StatusCode, e)
	}
}

// A label set can match a physical device without literally containing
// "device": the post-grant check must still refuse the reset (and release
// the lease so the device stays available).
func TestAcquireRejectsResetOnLabelMatchedDevice(t *testing.T) {
	ts := newDeviceTestServer(t, &fakeEngine{})
	var e proto.Error
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"iphone-15-pro"}, AgentID: "a1", Reset: "erase"}, &e)
	if resp.StatusCode != http.StatusBadRequest || e.Code != proto.ErrBadRequest {
		t.Fatalf("got %d %+v", resp.StatusCode, e)
	}
	var l proto.Lease
	resp = doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"iphone-15-pro"}, AgentID: "a1"}, &l)
	if resp.StatusCode != http.StatusCreated || l.State != proto.LeaseActive || l.TargetUDID != "DEV-1" {
		t.Fatalf("refused lease was not released: got %d %+v", resp.StatusCode, l)
	}
}

func TestAcquireDeviceLeaseWithoutResetGrants(t *testing.T) {
	ts := newDeviceTestServer(t, &fakeEngine{})
	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{Labels: []string{"device"}, AgentID: "a1"}, &l)
	if resp.StatusCode != http.StatusCreated || l.State != proto.LeaseActive || l.TargetUDID != "DEV-1" {
		t.Fatalf("got %d %+v", resp.StatusCode, l)
	}
}

// Explicit state-engine ops are simulator-only: a lease holding a
// physical device gets an honest 501 instead of an opaque simctl error.
func TestStateOpsRefusedOnDeviceLease(t *testing.T) {
	ts := newDeviceTestServer(t, &fakeEngine{})
	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{UDID: "DEV-1", AgentID: "a1"}, &l)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("lease: got %d %+v", resp.StatusCode, l)
	}
	var e proto.Error
	resp = doJSON(t, "POST", ts.URL+"/v0/state/erase",
		proto.EraseRequest{LeaseID: l.ID}, &e)
	if resp.StatusCode != http.StatusNotImplemented || e.Code != proto.ErrNotImplemented {
		t.Fatalf("erase: got %d %+v", resp.StatusCode, e)
	}
}

func TestAcquireSimResetStillWorks(t *testing.T) {
	ts := newDeviceTestServer(t, &fakeEngine{})
	var l proto.Lease
	resp := doJSON(t, "POST", ts.URL+"/v0/leases",
		proto.AcquireLeaseRequest{UDID: "SIM-1", AgentID: "a1", Reset: "erase"}, &l)
	if resp.StatusCode != http.StatusCreated || l.State != proto.LeaseActive {
		t.Fatalf("got %d %+v", resp.StatusCode, l)
	}
}
