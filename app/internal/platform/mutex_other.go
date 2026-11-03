//go:build !windows

package platform

// HoldAppMutex is only needed on Windows, where the installer looks for it.
func HoldAppMutex() (alreadyRunning bool) { return false }

// ShowAlreadyRunning is the box of Windows, where a second start is refused
// with a word. Elsewhere the running program shows itself instead.
func ShowAlreadyRunning() {}
