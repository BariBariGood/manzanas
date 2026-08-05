package client

import (
	"context"
	"net/http"

	"github.com/BariBariGood/manzanas/proto"
)

// StateSnapshot calls POST /v0/state/snapshots. The target UDID is derived
// server-side from the lease.
func (c *Client) StateSnapshot(ctx context.Context, leaseID, label string) (proto.SnapshotInfo, error) {
	var out proto.SnapshotInfo
	err := c.do(ctx, http.MethodPost, "/v0/state/snapshots",
		proto.SnapshotRequest{LeaseID: leaseID, Label: label}, &out)
	return out, err
}

// StateRestore calls POST /v0/state/restore; snapshot is an ID or label.
func (c *Client) StateRestore(ctx context.Context, leaseID, snapshot string, reboot bool) (proto.RestoreResult, error) {
	var out proto.RestoreResult
	err := c.do(ctx, http.MethodPost, "/v0/state/restore",
		proto.RestoreRequest{LeaseID: leaseID, Snapshot: snapshot, Reboot: reboot}, &out)
	return out, err
}

// StateFixture calls POST /v0/state/fixtures with an opaque payload owned
// by the state slice (e.g. "statusbar", "privacy", "locale").
func (c *Client) StateFixture(ctx context.Context, leaseID, name string, payload map[string]any) error {
	return c.do(ctx, http.MethodPost, "/v0/state/fixtures",
		proto.FixtureRequest{LeaseID: leaseID, Name: name, Payload: payload}, nil)
}
