package stream

import "context"

// FrameSource produces JPEG-encoded frames for one target. Implementations
// are pull-based: the hub calls Next at the configured capture rate.
type FrameSource interface {
	// Next returns the next JPEG frame. It blocks until a frame is
	// available or ctx is cancelled.
	Next(ctx context.Context) ([]byte, error)
	// Close releases capture resources.
	Close() error
}

// SourceFactory opens a FrameSource for the target identified by udid.
type SourceFactory func(udid string) (FrameSource, error)
