//go:build windows

package platform

import "syscall"

var procSeq = syscall.NewLazyDLL("user32.dll").NewProc("GetClipboardSequenceNumber")

// ChangeSeq returns the clipboard sequence number, which Windows bumps on
// every change.
func ChangeSeq() int64 {
	r, _, _ := procSeq.Call()
	return int64(r)
}
