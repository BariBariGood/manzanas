package eval

import "context"

// bootOp boots the leased target and waits until it reports Booted.
type bootOp struct{}

func (bootOp) Execute(ctx context.Context, rc *runContext, _ *Step) (string, error) {
	if err := rc.client.Boot(ctx, rc.udid, rc.lease); err != nil {
		return "", err
	}
	return "booted " + rc.udid, nil
}
