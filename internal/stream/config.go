package stream

import "time"

// Config bounds the streamer's resource usage. Zero values fall back to the
// defaults below.
type Config struct {
	// MaxStreams caps concurrently open streams across all targets.
	MaxStreams int
	// MaxViewers caps concurrent viewers attached to one stream.
	MaxViewers int
	// DefaultFPS is the capture rate when the request doesn't specify one.
	DefaultFPS int
	// MaxFPS clamps requested capture rates.
	MaxFPS int
	// Linger is how long a stream keeps capturing after its last viewer
	// detaches before the capture pump stops.
	Linger time.Duration
}

const (
	DefaultMaxStreams = 8
	DefaultMaxViewers = 16
	DefaultFPS        = 10
	DefaultMaxFPS     = 30
	DefaultLinger     = 10 * time.Second
)

func (c Config) withDefaults() Config {
	if c.MaxStreams <= 0 {
		c.MaxStreams = DefaultMaxStreams
	}
	if c.MaxViewers <= 0 {
		c.MaxViewers = DefaultMaxViewers
	}
	if c.DefaultFPS <= 0 {
		c.DefaultFPS = DefaultFPS
	}
	if c.MaxFPS <= 0 {
		c.MaxFPS = DefaultMaxFPS
	}
	if c.Linger <= 0 {
		c.Linger = DefaultLinger
	}
	return c
}

func (c Config) clampFPS(fps int) int {
	if fps <= 0 {
		fps = c.DefaultFPS
	}
	if fps > c.MaxFPS {
		fps = c.MaxFPS
	}
	return fps
}
