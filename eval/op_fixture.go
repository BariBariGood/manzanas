package eval

import (
	"context"

	"github.com/BariBariGood/manzanas/proto"
)

// fixtureOp applies a named state fixture (statusbar, privacy, push,
// locale, timezone, url) to the leased target.
type fixtureOp struct{}

func (fixtureOp) Execute(ctx context.Context, rc *runContext, st *Step) (string, error) {
	err := rc.client.Fixture(ctx, proto.FixtureRequest{
		LeaseID: rc.lease,
		Name:    st.Fixture,
		Payload: st.FixturePayload,
	})
	if err != nil {
		return "", err
	}
	return "fixture " + st.Fixture, nil
}
