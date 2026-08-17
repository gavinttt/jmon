//go:build !windows

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // new session = daemon
	}
}

func openBrowserAsync(url string) {
	cmd := exec.Command("sh", "-c", fmt.Sprintf("sleep 1 && open %q 2>/dev/null || xdg-open %q 2>/dev/null", url, url))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Start()
}

func stopProcess(proc *os.Process) error {
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		proc.Kill()
	}
	return nil
}

func isProcessAlive(proc *os.Process) bool {
	return proc.Signal(syscall.Signal(0)) == nil
}
