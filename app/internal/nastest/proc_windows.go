package nastest

import (
	"os/exec"
	"strconv"
)

func ownProcessGroup(cmd *exec.Cmd) {}

// taskkill /T also stops the workerd that wrangler started.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	}
}
