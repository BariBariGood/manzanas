package eval

import (
	"context"
	"time"
)

// waitOp sleeps for a fixed duration (settle time between UI actions).
type waitOp struct{}

func (waitOp) Execute(ctx context.Context, _ *runContext, st *Step) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(st.Duration.Std()):
	}
	return "waited " + st.Duration.Std().String(), nil
}
