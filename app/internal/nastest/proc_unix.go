//go:build !windows

package nastest

import (
	"os/exec"
	"syscall"
)

// wrangler starts workerd as a child: run it in its own process group so
// that stopping the test stops both.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
