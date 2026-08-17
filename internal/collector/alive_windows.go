//go:build windows

package collector

import (
	"fmt"
	"os/exec"
	"strings"
)

// IsProcessAlive checks if a process with the given PID still exists on Windows
func IsProcessAlive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
	if err != nil {
		return false
	}
	return !strings.Contains(string(out), "INFO: No tasks are running")
}
