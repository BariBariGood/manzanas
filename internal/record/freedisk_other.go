//go:build !darwin && !linux

package record

import "math"

func freeDisk(path string) (int64, error) { return math.MaxInt64, nil }
