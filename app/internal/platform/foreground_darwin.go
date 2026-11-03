//go:build darwin

package platform

import (
	"runtime"
	"unsafe"

	"klip/internal/capture"
)

// ForegroundApp returns the program in front (NSWorkspace's
// frontmostApplication): its name and the path of its bundle. nil when it
// cannot be told. Call it when the clipboard changed, not on every poll.
func ForegroundApp() *capture.AppIdentity {
	if !objcReady {
		return nil
	}
	// The objects below are autoreleased; this may run on a goroutine that
	// has no pool, so make one. A pool belongs to the thread that made it,
	// so the goroutine stays on its thread until the pool is drained.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	pool := msgSend(msgSend(getClass("NSAutoreleasePool"), selReg("alloc")), selReg("init"))
	defer msgSend(pool, selReg("drain"))

	ws := msgSend(getClass("NSWorkspace"), selReg("sharedWorkspace"))
	app := msgSend(ws, selReg("frontmostApplication"))
	if app == 0 {
		return nil
	}
	name := nsString(msgSend(app, selReg("localizedName")))
	path := ""
	if url := msgSend(app, selReg("bundleURL")); url != 0 {
		path = nsString(msgSend(url, selReg("path")))
	}
	if name == "" && path == "" {
		return nil
	}
	return &capture.AppIdentity{Name: name, Path: path}
}

// nsString copies an NSString into a Go string (UTF-8).
func nsString(s uintptr) string {
	if s == 0 {
		return ""
	}
	const utf8 = 4 // NSUTF8StringEncoding
	n := int(msgSend1(s, selReg("lengthOfBytesUsingEncoding:"), utf8))
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n+1)
	if msgSend3(s, selReg("getCString:maxLength:encoding:"), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), utf8) == 0 {
		return ""
	}
	return string(buf[:n])
}
