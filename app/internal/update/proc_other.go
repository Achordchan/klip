//go:build !windows

package update

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// waitExit polls until the process is gone, or for timeout at most (none:
// for ever), and says whether it is gone.
func waitExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			return true
		}
		if timeout > 0 && !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// launch starts a program in its own session and does not wait for it.
func launch(path string) error {
	cmd := exec.Command(path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// StartHelper starts the helper program with the plan.
func StartHelper(helperExe, planPath string) error {
	cmd, err := startHelperProcess(helperExe, planPath)
	if err == nil {
		go cmd.Wait()
	}
	return err
}

func startHelperProcess(helperExe, planPath string) (*exec.Cmd, error) {
	cmd := exec.Command(helperExe, "--apply-update", planPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func alert(title, msg string) { fmt.Fprintln(os.Stderr, title+": "+msg) }

// launchInstaller starts the installer: no shell to ask for rights here, so it
// is started as any program.
func launchInstaller(path string) error { return launch(path) }
