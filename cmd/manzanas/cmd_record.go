package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/BariBariGood/manzanas/proto"
)

// cmdRecord implements `manzanas record start|stop`.
func cmdRecord(ctx context.Context, app *appEnv, args []string) error {
	sub, err := requireArg(args, 0, "record subcommand (start|stop)")
	if err != nil {
		return err
	}
	switch sub {
	case "start":
		return recordStart(ctx, app, args[1:])
	case "stop":
		return recordStop(ctx, app, args[1:])
	default:
		return fmt.Errorf("unknown record subcommand %q", sub)
	}
}

// leaseTarget resolves the lease's target UDID for the target-scoped
// recording endpoints.
func leaseTarget(ctx context.Context, app *appEnv, leaseID string) (string, error) {
	if leaseID == "" {
		return "", fmt.Errorf("--lease is required (or set MANZANAS_LEASE)")
	}
	l, err := app.client.GetLease(ctx, leaseID)
	if err != nil {
		return "", err
	}
	if l.TargetUDID == "" {
		return "", fmt.Errorf("lease %s holds no target (state %s)", leaseID, l.State)
	}
	return l.TargetUDID, nil
}

// recordStart maps to POST /v0/targets/{udid}/recording.
func recordStart(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("record start")
	lease := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID")
	codec := fs.String("codec", "", "video codec: hevc (default) or h264")
	maxSeconds := fs.Int("max-seconds", 0, "duration cap in seconds (default: daemon cap)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	udid, err := leaseTarget(ctx, app, *lease)
	if err != nil {
		return err
	}
	rec, err := app.client.StartRecording(ctx, udid, proto.RecordingRequest{
		LeaseID: *lease, Codec: *codec, MaxSeconds: *maxSeconds,
	})
	if err != nil {
		return err
	}
	return app.emit(rec, func(w io.Writer) {
		fmt.Fprintf(w, "recording %s started on %s (%s, max %ds)\n",
			rec.ID, rec.TargetUDID, rec.Codec, rec.MaxSeconds)
	})
}

// recordStop maps to POST /v0/targets/{udid}/recording/stop.
func recordStop(ctx context.Context, app *appEnv, args []string) error {
	fs := app.newFlagSet("record stop")
	lease := fs.String("lease", os.Getenv("MANZANAS_LEASE"), "active lease ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	udid, err := leaseTarget(ctx, app, *lease)
	if err != nil {
		return err
	}
	res, err := app.client.StopRecording(ctx, udid, *lease)
	if err != nil {
		return err
	}
	return app.emit(res, func(w io.Writer) {
		if res.Artifact != nil {
			fmt.Fprintf(w, "recording %s stopped: %.1fs, %d bytes (%s)\n",
				res.RecordingID, res.DurationS, res.Bytes, res.Codec)
			fmt.Fprintf(w, "artifact: %s (sha256 %s)\n", res.Artifact.Path, res.Artifact.SHA256)
			fmt.Fprintf(w, "download: GET /v0/journal/%s/%s\n", *lease, res.Artifact.Path)
		} else {
			fmt.Fprintf(w, "recording %s stopped (%s) but produced no artifact\n",
				res.RecordingID, res.Reason)
		}
	})
}
