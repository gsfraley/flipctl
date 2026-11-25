//go:build unix

package runner

import (
	"os/exec"
	"syscall"
)

// killProcessGroupOnCancel runs the command in its own process group and, when
// its context ends, kills the whole group. Otherwise a canceled shell script's
// children (a sleep, a tool it spawned) keep running and keep its output pipe
// open after the parent is gone.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
