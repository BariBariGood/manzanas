package eval

import "context"

// shutdownOp shuts the leased target down and waits until it reports
// Shutdown.
type shutdownOp struct{}

func (shutdownOp) Execute(ctx context.Context, rc *runContext, _ *Step) (string, error) {
	if err := rc.client.Shutdown(ctx, rc.udid, rc.lease); err != nil {
		return "", err
	}
	return "shutdown " + rc.udid, nil
}
