//go:build !windows

package autostart

// Default is the autostart of this platform: an XDG entry. (macOS uses
// MyGo's login item, see platform.Autostart.)
func Default() Service { return Desktop{} }
