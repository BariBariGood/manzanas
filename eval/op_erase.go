package eval

import (
	"context"

	"github.com/BariBariGood/manzanas/proto"
)

// eraseOp factory-resets the leased (shutdown) target.
type eraseOp struct{}

func (eraseOp) Execute(ctx context.Context, rc *runContext, _ *Step) (string, error) {
	if err := rc.client.Erase(ctx, proto.EraseRequest{LeaseID: rc.lease}); err != nil {
		return "", err
	}
	return "erased", nil
}
