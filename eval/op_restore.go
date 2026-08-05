package eval

import (
	"context"
	"fmt"

	"github.com/BariBariGood/manzanas/proto"
)

// restoreOp restores the leased target to a snapshot (by ID or label).
type restoreOp struct{}

func (restoreOp) Execute(ctx context.Context, rc *runContext, st *Step) (string, error) {
	res, err := rc.client.Restore(ctx, proto.RestoreRequest{
		LeaseID:  rc.lease,
		Snapshot: st.Snapshot,
		Reboot:   st.Reboot,
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("restored %s rebooted=%v", st.Snapshot, res.Rebooted), nil
}
