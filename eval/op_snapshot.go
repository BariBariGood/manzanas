package eval

import (
	"context"

	"github.com/BariBariGood/manzanas/proto"
)

// snapshotOp captures a labelled snapshot of the leased (shutdown) target.
type snapshotOp struct{}

func (snapshotOp) Execute(ctx context.Context, rc *runContext, st *Step) (string, error) {
	info, err := rc.client.Snapshot(ctx, proto.SnapshotRequest{
		LeaseID: rc.lease,
		Label:   st.SnapshotLabel,
	})
	if err != nil {
		return "", err
	}
	rc.snapshots = append(rc.snapshots, info.ID)
	return "snapshot " + info.ID + " label=" + st.SnapshotLabel, nil
}
