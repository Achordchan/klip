//go:build windows

package platform

import (
	"syscall"
	"unsafe"
)

var procCreateMutexW = syscall.NewLazyDLL("kernel32.dll").NewProc("CreateMutexW")

// appMutex is kept for the life of the process.
var appMutex uintptr

// HoldAppMutex takes the named mutex that 1.0.10 and older take and that the
// installer's AppMutex setting looks for ("maccy_mutex"): while it exists the
// installer knows the program runs and asks it to close. It says whether
// another instance already holds it.
func HoldAppMutex() (alreadyRunning bool) {
	name, _ := syscall.UTF16PtrFromString("maccy_mutex")
	h, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return false
	}
	appMutex = h
	const errorAlreadyExists = syscall.Errno(183)
	return err == errorAlreadyExists
}

var procMessageBoxW = syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")

// ShowAlreadyRunning tells the user that Klip runs already, in the box the
// .NET client shows (MB_OK, information icon, in front of everything).
func ShowAlreadyRunning() {
	text, _ := syscall.UTF16PtrFromString("Klip 已在运行，禁止多开。")
	title, _ := syscall.UTF16PtrFromString("Klip")
	const mbOK, mbIconInformation, mbSetForeground, mbTopmost = 0x0, 0x40, 0x10000, 0x40000
	_, _, _ = procMessageBoxW.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)),
		uintptr(mbOK|mbIconInformation|mbSetForeground|mbTopmost))
}
