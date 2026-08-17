//go:build windows

package daemon

import (
	"os"
	"os/exec"
	"syscall"
)

func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

func openBrowserAsync(url string) {
	exec.Command("cmd", "/c", "start", url).Start()
}

func stopProcess(proc *os.Process) error {
	return proc.Kill()
}

func isProcessAlive(proc *os.Process) bool {
	return proc.Signal(syscall.Signal(0)) == nil
}
