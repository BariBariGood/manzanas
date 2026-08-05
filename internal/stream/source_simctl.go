package stream

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// SimctlSource captures frames from a booted simulator by invoking
// `xcrun simctl io <udid> screenshot` per frame. Simpler and far more robust
// than parsing an H.264 elementary stream out of `recordVideo`; the hub's
// capture pump paces calls at the configured fps (see docs/streaming.md for
// the trade-off discussion).
type SimctlSource struct {
	udid string
	dir  string
}

// NewSimctlSource returns a screenshot-based FrameSource for udid.
func NewSimctlSource(udid string) (*SimctlSource, error) {
	dir, err := os.MkdirTemp("", "manzanasd-stream-"+udid)
	if err != nil {
		return nil, err
	}
	return &SimctlSource{udid: udid, dir: dir}, nil
}

// SimctlSourceFactory is a SourceFactory producing SimctlSources. On
// non-macOS hosts it fails; callers should select FakeSourceFactory there.
func SimctlSourceFactory(udid string) (FrameSource, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("simctl capture requires macOS")
	}
	return NewSimctlSource(udid)
}

func (s *SimctlSource) Next(ctx context.Context) ([]byte, error) {
	// simctl only writes to a file/pty, not a plain pipe, so round-trip
	// through a temp file. JPEG keeps per-frame encode + transfer cheap.
	path := filepath.Join(s.dir, "frame.jpeg")
	cmd := exec.CommandContext(ctx, "xcrun", "simctl", "io", s.udid,
		"screenshot", "--type=jpeg", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("simctl screenshot %s: %v: %s", s.udid, err, out)
	}
	return os.ReadFile(path)
}

func (s *SimctlSource) Close() error {
	return os.RemoveAll(s.dir)
}
