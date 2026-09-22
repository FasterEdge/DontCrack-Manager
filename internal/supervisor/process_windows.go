//go:build windows

package supervisor

import (
	"os/exec"
	"strconv"
	"syscall"
)

func newSysProcAttr() *syscall.SysProcAttr {
	return nil
}

// Windows has no POSIX process groups. Kill the process tree by PID when the
// supervisor reaches its force-kill fallback; graceful shutdown is handled by
// the child process itself.
func signalProcessGroup(pgid int, sig syscall.Signal) error {
	if sig != syscall.SIGKILL {
		return nil
	}
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pgid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}
