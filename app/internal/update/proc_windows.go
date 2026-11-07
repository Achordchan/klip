//go:build windows

package update

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

const (
	synchronize     = 0x00100000
	infinite        = 0xFFFFFFFF
	waitObject0     = 0x00000000
	createNoWindow  = 0x08000000
	detachedProcess = 0x00000008
	mbOK            = 0x0
	mbIconError     = 0x10
)

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	user32          = syscall.NewLazyDLL("user32.dll")
	procOpenProcess = kernel32.NewProc("OpenProcess")
	procWait        = kernel32.NewProc("WaitForSingleObject")
	procCloseHandle = kernel32.NewProc("CloseHandle")
	procMessageBoxW = user32.NewProc("MessageBoxW")
	procShellExecW  = syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")
)

// waitExit blocks until the process is gone, or for timeout at most (none:
// for ever), and says whether it is gone; one that cannot be opened is taken
// to be gone already.
func waitExit(pid int, timeout time.Duration) bool {
	h, _, _ := procOpenProcess.Call(synchronize, 0, uintptr(pid))
	if h == 0 {
		return true
	}
	defer procCloseHandle.Call(h)
	ms := uintptr(infinite)
	if timeout > 0 {
		ms = uintptr(timeout / time.Millisecond)
	}
	r, _, _ := procWait.Call(h, ms)
	return r == waitObject0
}

// launch starts a program, detached and without a console window. Its own
// windows are left as the program wants them: this starts the installer
// wizard too, and a window hidden by the start-up information (SW_HIDE) would
// leave it running where nobody can see it.
func launch(path string) error {
	cmd := exec.Command(path)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow | detachedProcess}
	return cmd.Start()
}

// StartHelper starts the helper program with the plan, hidden.
func StartHelper(helperExe, planPath string) error {
	cmd, err := startHelperProcess(helperExe, planPath)
	if err == nil {
		go cmd.Wait()
	}
	return err
}

func startHelperProcess(helperExe, planPath string) (*exec.Cmd, error) {
	cmd := exec.Command(helperExe, "--apply-update", planPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow | detachedProcess}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func alert(title, msg string) {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(msg)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), mbOK|mbIconError)
}

// launchInstaller starts the installer through the shell, which shows the
// prompt for administrator rights that an installer asks for; starting it as a
// plain process fails with "requires elevation".
func launchInstaller(path string) error {
	verb, _ := syscall.UTF16PtrFromString("open")
	file, _ := syscall.UTF16PtrFromString(path)
	const swShowNormal = 1
	r, _, err := procShellExecW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), 0, 0, swShowNormal)
	if r <= 32 { // ShellExecute reports success with a value above 32
		if err == nil || err == syscall.Errno(0) {
			err = fmt.Errorf("ShellExecute returned %d", r)
		}
		return err
	}
	return nil
}
