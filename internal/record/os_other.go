//go:build !darwin

package record

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func cmdlineOS(pid int) string {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(raw), "\x00", " ")
}

// findRecordVideoPids: no simctl off macOS; nothing to sweep.
func findRecordVideoPids() []int { return nil }
