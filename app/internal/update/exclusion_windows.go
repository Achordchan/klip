//go:build windows

package update

import (
	"golang.org/x/sys/windows"
	"sync"
)

// The helper creates this marker before waiting for the old process to exit.
// Kernel ownership releases it even if the helper crashes.
func holdUpdateExclusion() (func(), error) {
	name, _ := windows.UTF16PtrFromString("klip_update_in_progress")
	h, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		return nil, err
	}
	legacy, _ := windows.UTF16PtrFromString("maccy_mutex")
	app, appErr := windows.CreateMutex(nil, false, legacy)
	if appErr != nil && appErr != windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(h)
		return nil, appErr
	}
	var once sync.Once
	return func() { once.Do(func() { windows.CloseHandle(app); windows.CloseHandle(h) }) }, nil
}
func UpdateInProgress() bool {
	name, _ := windows.UTF16PtrFromString("klip_update_in_progress")
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, name)
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}
