//go:build darwin

package platform

import "github.com/ebitengine/purego"

// ChangeSeq returns NSPasteboard's changeCount, which grows on every
// clipboard change, or -1 when it cannot be read.
func ChangeSeq() int64 {
	if !objcReady {
		return -1
	}
	return int64(msgSend(pbID, pbCountSel))
}

var (
	objcReady  bool
	msgSend    func(obj, sel uintptr) uintptr
	msgSend1   func(obj, sel, a uintptr) uintptr
	msgSend3   func(obj, sel, a, b, c uintptr) uintptr
	getClass   func(name string) uintptr
	selReg     func(name string) uintptr
	pbID       uintptr
	pbCountSel uintptr
)

func init() {
	objc, err := purego.Dlopen("/usr/lib/libobjc.A.dylib", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return
	}
	if _, err := purego.Dlopen("/System/Library/Frameworks/AppKit.framework/AppKit", purego.RTLD_LAZY|purego.RTLD_GLOBAL); err != nil {
		return
	}
	purego.RegisterLibFunc(&getClass, objc, "objc_getClass")
	purego.RegisterLibFunc(&selReg, objc, "sel_registerName")
	purego.RegisterLibFunc(&msgSend, objc, "objc_msgSend")
	purego.RegisterLibFunc(&msgSend1, objc, "objc_msgSend")
	purego.RegisterLibFunc(&msgSend3, objc, "objc_msgSend")
	cls := getClass("NSPasteboard")
	if cls == 0 {
		return
	}
	// generalPasteboard is a process-wide singleton: ask once and keep it, so
	// polling from a goroutine allocates nothing autoreleased.
	pbID = msgSend(cls, selReg("generalPasteboard"))
	pbCountSel = selReg("changeCount")
	objcReady = pbID != 0
}
