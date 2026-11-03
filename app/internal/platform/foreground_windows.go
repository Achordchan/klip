//go:build windows

package platform

import (
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"klip/internal/capture"
)

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procForegroundWindow = user32.NewProc("GetForegroundWindow")
	procWindowThreadPID  = user32.NewProc("GetWindowThreadProcessId")
	procOpenProcess      = kernel32.NewProc("OpenProcess")
	procQueryImageName   = kernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle      = kernel32.NewProc("CloseHandle")
)

const processQueryLimitedInformation = 0x1000

// ForegroundApp returns the program owning the window in front, as
// App.axaml.cs TryGetForegroundApp does: the path of its executable and the
// file name without extension. A program whose path cannot be read (some
// system processes) gives nil, where C# would give the bare process name.
func ForegroundApp() *capture.AppIdentity {
	hwnd, _, _ := procForegroundWindow.Call()
	if hwnd == 0 {
		return nil
	}
	var pid uint32
	procWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return nil
	}
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return nil
	}
	defer procCloseHandle.Call(h)
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	if r, _, _ := procQueryImageName.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r == 0 {
		return nil
	}
	path := syscall.UTF16ToString(buf[:n])
	if path == "" {
		return nil
	}
	base := filepath.Base(path)
	return &capture.AppIdentity{Name: strings.TrimSuffix(base, filepath.Ext(base)), Path: path}
}
