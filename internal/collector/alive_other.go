//go:build !linux && !windows

package collector

import (
	"os"
	"syscall"
)

// IsProcessAlive checks if a process with the given PID still exists
func IsProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
