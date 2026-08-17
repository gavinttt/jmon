//go:build linux

package collector

import (
	"fmt"
	"os"
)

// IsProcessAlive checks if a process with the given PID still exists
func IsProcessAlive(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}
