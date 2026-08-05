package record

import (
	"os/exec"
	"strconv"
	"strings"
)

func cmdlineOS(pid int) string {
	// -ww: never truncate the command; the spool path must survive.
	out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// findRecordVideoPids lists live `simctl io ... recordVideo` processes.
func findRecordVideoPids() []int {
	out, err := exec.Command("pgrep", "-f", "simctl io .* recordVideo").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(line); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}
