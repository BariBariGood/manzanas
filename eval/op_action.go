package eval

import (
	"context"
	"fmt"

	"github.com/BariBariGood/manzanas/proto"
)

// actionOp dispatches one ActionRequest (tap, swipe, type, observe,
// screenshot, launch_app, ...) for the leased target.
type actionOp struct{}

func (actionOp) Execute(ctx context.Context, rc *runContext, st *Step) (string, error) {
	res, err := rc.client.Action(ctx, proto.ActionRequest{
		LeaseID: rc.lease,
		Kind:    st.Kind,
		Payload: rc.profile.scaleWaitPayload(st.Kind, st.Payload),
	})
	if err != nil {
		return "", err
	}
	if !res.OK {
		return "", fmt.Errorf("action %s returned ok=false", st.Kind)
	}
	return st.Kind, nil
}
