// Package stream defines the interface contract for the H.264/MJPEG media
// streamer. The implementation is owned by the "stream" slice.
package stream

import (
	"context"

	"github.com/BariBariGood/manzanas/proto"
)

// Streamer negotiates and serves media streams for booted targets. The
// returned StreamOffer.URL is a WS endpoint the streamer itself serves;
// multiple viewers may attach to one stream.
type Streamer interface {
	// Open negotiates a stream for the target identified by udid. The lease
	// has already been validated by the server.
	Open(ctx context.Context, udid string, req proto.StreamRequest) (proto.StreamOffer, error)
	// Close tears down a previously opened stream.
	Close(ctx context.Context, streamID string) error
}
