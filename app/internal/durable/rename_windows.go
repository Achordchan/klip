//go:build windows

package durable

import "golang.org/x/sys/windows"

// Rename waits for the replacement to be written through before returning.
func Rename(from, to string) error {
	f, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(f, t, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// Windows publishes the directory tree through the write-through move.
func syncDirectory(string) error { return nil }
