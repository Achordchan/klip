//go:build !windows

package main

import "time"

// systemIdle: updates install by themselves on Windows only, so elsewhere
// the user is never taken to be away.
func systemIdle() time.Duration { return 0 }
