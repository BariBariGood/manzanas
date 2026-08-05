package warm

import (
	"errors"
	"fmt"
)

const (
	// DefaultLoadFactor gates boots when 1-min load > factor * cores.
	DefaultLoadFactor = 2.0
	// DefaultMinFreeDisk refuses boot/create below 5 GiB free: sims wedge
	// silently once the volume drops under ~35 MB, so stop far earlier.
	DefaultMinFreeDisk = uint64(5 << 30)
)

// ErrLoadTooHigh is returned when the load gate refuses a boot.
var ErrLoadTooHigh = errors.New("host load too high for a simulator boot")

// ErrDiskTooLow is returned when the free-space gate refuses a boot/create.
var ErrDiskTooLow = errors.New("free disk too low for a simulator boot")

// checkLoad enforces the load gate: 1-min load must be <= factor * cores.
func checkLoad(h Host, factor float64) error {
	load, err := h.LoadAvg1()
	if err != nil {
		return err
	}
	limit := factor * float64(h.NumCPU())
	if load > limit {
		return fmt.Errorf("%w: load %.1f > %.1f (%.0fx %d cores)", ErrLoadTooHigh, load, limit, factor, h.NumCPU())
	}
	return nil
}

// checkDisk enforces the free-space gate on the given path's volume.
func checkDisk(h Host, path string, minFree uint64) error {
	free, err := h.FreeDiskBytes(path)
	if err != nil {
		return err
	}
	if free < minFree {
		return fmt.Errorf("%w: %.1f GiB free < %.1f GiB required", ErrDiskTooLow,
			float64(free)/(1<<30), float64(minFree)/(1<<30))
	}
	return nil
}
