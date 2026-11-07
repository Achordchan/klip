//go:build windows

package main

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	procGetLastInputInfo = syscall.NewLazyDLL("user32.dll").NewProc("GetLastInputInfo")
	procGetTickCount     = syscall.NewLazyDLL("kernel32.dll").NewProc("GetTickCount")
)

type lastInputInfo struct {
	size uint32
	time uint32
}

// systemIdle is how long ago the user last used mouse or keyboard, in this
// session (GetLastInputInfo). 0 when it cannot be told: never idle.
func systemIdle() time.Duration {
	info := lastInputInfo{size: uint32(unsafe.Sizeof(lastInputInfo{}))}
	if r, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		return 0
	}
	now, _, _ := procGetTickCount.Call()
	// Both are milliseconds since the start, in 32 bits: the difference is
	// right across the wrap after 49.7 days.
	return time.Duration(uint32(now)-info.time) * time.Millisecond
}
