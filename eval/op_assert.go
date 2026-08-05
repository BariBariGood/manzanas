package eval

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BariBariGood/manzanas/proto"
)

// errTerminal marks assertion failures that cannot become true by
// retrying (misconfiguration, artifact write errors); the retry loop
// fails the step immediately instead of burning the step's time budget.
var errTerminal = errors.New("terminal assertion error")

func terminalf(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, errTerminal)...)
}

// assertRetryInterval is the poll interval for assertions, which retry
// until they hold or the step timeout expires (UI trees settle
// asynchronously, and observe backends can be transiently unavailable
// right after an app launch).
const assertRetryInterval = 2 * time.Second

// assertOp evaluates one assertion against the live target: element
// presence/absence via observe, tree-hash record/compare, or screenshot
// capture. The assertion is polled until it holds or the step's context
// expires; the last failure is reported.
type assertOp struct{}

func (assertOp) Execute(ctx context.Context, rc *runContext, st *Step) (string, error) {
	for {
		detail, err := evalAssertion(ctx, rc, st)
		if err == nil {
			return detail, nil
		}
		if errors.Is(err, errTerminal) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", err
		case <-time.After(assertRetryInterval):
		}
		if ctx.Err() != nil {
			return "", err
		}
	}
}

func evalAssertion(ctx context.Context, rc *runContext, st *Step) (string, error) {
	a := st.Assert
	switch {
	case a.ElementExists != nil:
		found, _, err := rc.observeFind(ctx, a.ElementExists)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("element not found: %s", a.ElementExists)
		}
		return "element exists: " + a.ElementExists.String(), nil

	case a.ElementAbsent != nil:
		found, _, err := rc.observeFind(ctx, a.ElementAbsent)
		if err != nil {
			return "", err
		}
		if found {
			return "", fmt.Errorf("element unexpectedly present: %s", a.ElementAbsent)
		}
		return "element absent: " + a.ElementAbsent.String(), nil

	case a.TreeHash != nil:
		return rc.assertTreeHash(ctx, a.TreeHash)

	case a.Screenshot != nil:
		return rc.assertScreenshot(ctx, a.Screenshot)
	}
	return "", terminalf("empty assertion")
}

// observeFind runs an observe action and searches the tree for the query.
func (rc *runContext) observeFind(ctx context.Context, q *ElementQuery) (bool, string, error) {
	res, err := rc.client.Action(ctx, proto.ActionRequest{
		LeaseID: rc.lease,
		Kind:    "observe",
	})
	if err != nil {
		return false, "", err
	}
	hash, _ := res.Result["hash"].(string)
	tree, ok := res.Result["tree"]
	if !ok {
		return false, hash, fmt.Errorf("observe result has no tree")
	}
	return findElement(tree, q), hash, nil
}

func (rc *runContext) assertTreeHash(ctx context.Context, th *TreeHashAssertion) (string, error) {
	res, err := rc.client.Action(ctx, proto.ActionRequest{
		LeaseID: rc.lease,
		Kind:    "observe",
	})
	if err != nil {
		return "", err
	}
	hash, _ := res.Result["hash"].(string)
	if hash == "" {
		return "", fmt.Errorf("observe result has no hash")
	}
	switch {
	case th.SaveAs != "":
		rc.saved[th.SaveAs] = hash
		return fmt.Sprintf("tree hash %s saved as %q", short(hash), th.SaveAs), nil
	case th.EqualsSaved != "":
		want, ok := rc.saved[th.EqualsSaved]
		if !ok {
			return "", terminalf("no saved tree hash named %q", th.EqualsSaved)
		}
		if hash != want {
			return "", fmt.Errorf("tree hash mismatch: got %s, saved %q is %s", short(hash), th.EqualsSaved, short(want))
		}
		return fmt.Sprintf("tree hash %s == saved %q", short(hash), th.EqualsSaved), nil
	default:
		if hash != th.Equals {
			return "", fmt.Errorf("tree hash mismatch: got %s, want %s", short(hash), short(th.Equals))
		}
		return "tree hash == " + short(hash), nil
	}
}

func (rc *runContext) assertScreenshot(ctx context.Context, sa *ScreenshotAssertion) (string, error) {
	res, err := rc.client.Action(ctx, proto.ActionRequest{
		LeaseID: rc.lease,
		Kind:    "screenshot",
		Payload: map[string]any{"inline": true},
	})
	if err != nil {
		return "", err
	}
	// Only the two formats the daemon can produce are trusted; anything
	// else (including a missing field) is treated as PNG so a hostile
	// daemon can't steer the artifact filename below.
	format, _ := res.Result["format"].(string)
	if format != "jpeg" {
		format = "png"
	}
	key := format + "_base64"
	b64, _ := res.Result[key].(string)
	if b64 == "" {
		return "", fmt.Errorf("screenshot result has no %s", key)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("decoding screenshot: %w", err)
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("screenshot is empty")
	}
	detail := fmt.Sprintf("screenshot captured (%d bytes)", len(raw))
	if sa.Save != "" && rc.artDir != "" {
		name := fmt.Sprintf("%s-%s-run%d.%s", rc.scenario, sa.Save, rc.run, format)
		path := filepath.Join(rc.artDir, name)
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			return "", terminalf("saving screenshot: %v", err)
		}
		rc.artifacts = append(rc.artifacts, path)
		detail += " -> " + name
	}
	return detail, nil
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
